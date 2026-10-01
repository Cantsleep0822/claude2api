package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"claude2api/internal/config"
	"claude2api/internal/repository"
	"claude2api/internal/utils"

	fhttp "github.com/bogdanfinn/fhttp"
)

const modelCatalogTTL = 10 * time.Minute

var modelCatalog = struct {
	sync.Mutex
	ids       []string
	expiresAt time.Time
}{}

// AvailableModelIDs returns models shared by all usable pool accounts.
func AvailableModelIDs() ([]string, error) {
	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if len(modelCatalog.ids) > 0 && time.Now().Before(modelCatalog.expiresAt) {
		return append([]string(nil), modelCatalog.ids...), nil
	}

	ids, err := refreshAvailableModelIDs()
	if err != nil {
		// 上游临时失败时，优先保留旧缓存。
		if len(modelCatalog.ids) > 0 {
			return append([]string(nil), modelCatalog.ids...), nil
		}
		return nil, err
	}

	modelCatalog.ids = append([]string(nil), ids...)
	modelCatalog.expiresAt = time.Now().Add(modelCatalogTTL)
	return append([]string(nil), ids...), nil
}

func refreshAvailableModelIDs() ([]string, error) {
	if !repository.Ready() {
		return nil, errors.New("数据库尚未初始化")
	}

	accounts := repository.LoadAccounts()
	var order []string
	var shared map[string]struct{}
	checked := 0

	for i := range accounts {
		account := &accounts[i]
		if !AccountUsable(account) {
			continue
		}

		ids, err := modelIDsForAccount(account)
		if err != nil {
			return nil, fmt.Errorf("读取账号模型配置失败: %w", err)
		}

		current := make(map[string]struct{}, len(ids))
		normalized := make([]string, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, exists := current[id]; !exists {
				current[id] = struct{}{}
				normalized = append(normalized, id)
			}
		}

		if len(current) == 0 {
			return nil, errors.New("账号未返回可用模型")
		}

		if checked == 0 {
			shared = current
			order = normalized
		} else {
			for id := range shared {
				if _, exists := current[id]; !exists {
					delete(shared, id)
				}
			}
		}
		checked++
	}

	if checked == 0 {
		return nil, errors.New("号池中没有可查询模型的账号")
	}

	result := make([]string, 0, len(shared))
	for _, id := range order {
		if _, exists := shared[id]; exists {
			result = append(result, id)
			delete(shared, id)
		}
	}

	if len(result) == 0 {
		return nil, errors.New("账号之间没有共同可用模型")
	}
	return result, nil
}

func modelIDsForAccount(account *repository.Account) ([]string, error) {
	lease := clientFor(account, config.Get().Proxy)
	lease.Lock()
	defer lease.Unlock()

	client := lease.ClaudeAI
	if !lease.ready {
		if err := client.WarmUp(); err != nil {
			return nil, err
		}
		lease.ready = true
	}

	if strings.TrimSpace(client.orgUUID) == "" {
		info, err := client.GetUserInfo()
		if err != nil {
			return nil, err
		}
		if info.OrgUUID == "" {
			return nil, errors.New("账号信息缺少组织 UUID")
		}
		repository.UpdateAccount(account.Email, func(a *repository.Account) {
			a.OrgUUID = info.OrgUUID
		})
	}

	return client.GetAvailableModels()
}

func (claudeAI *ClaudeAI) GetAvailableModels() ([]string, error) {
	if strings.TrimSpace(claudeAI.orgUUID) == "" {
		return nil, errors.New("账号缺少组织 UUID")
	}

	u, err := url.Parse(claudeAIBaseURL)
	if err == nil {
		claudeAI.client.SetCookies(u, []*fhttp.Cookie{{
			Name: "lastActiveOrg", Value: claudeAI.orgUUID, Domain: "claude.ai",
		}})
	}

	req, err := claudeAI.request(
		fhttp.MethodGet,
		fmt.Sprintf("%s/api/organizations/%s", claudeAIBaseURL, claudeAI.orgUUID),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("构造模型列表请求失败: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("referer", claudeAIBaseURL+"/new")

	resp, err := claudeAI.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询模型列表失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取模型列表失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return nil, fmt.Errorf(
			"查询模型列表 HTTP %d: %s",
			resp.StatusCode,
			utils.Truncate(string(body), 200),
		)
	}

	var org struct {
		Models []struct {
			Model    string `json:"model"`
			Inactive bool   `json:"inactive"`
			Overflow bool   `json:"overflow"`
		} `json:"claude_ai_bootstrap_models_config"`
	}

	if err := json.Unmarshal(body, &org); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}

	seen := map[string]struct{}{}
	models := make([]string, 0, len(org.Models))

	for _, item := range org.Models {
		id := strings.TrimSpace(item.Model)
		if id == "" || item.Inactive || item.Overflow {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}

	if len(models) == 0 {
		return nil, errors.New("上游未返回可用模型")
	}
	return models, nil
}
