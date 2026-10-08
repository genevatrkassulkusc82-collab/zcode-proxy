package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// ---- 下游网关 Key 管理 API（R1）----
// 明文只在创建响应出现一次；列表/详情只回 key_prefix。

func (s *APIServer) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.ListGatewayKeys()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keys == nil {
		keys = []*GatewayKey{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": keys})
}

func (s *APIServer) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		RPMLimit       int    `json:"rpm_limit"`
		QuotaTotal     int64  `json:"quota_total"`
		Models         string `json:"models"`
		Enabled        *bool  `json:"enabled"`
		VerifyPassword string `json:"verify_password"`
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 口令步进（与根 Key generate/reveal 同规）：创建即回一次明文，纯 session
	// 可调等于 stolen session 铸无限制命名 Key（空 models + 0 限额）。独立限速键
	// ip|api-key-create，与登录/根 Key 步进分桶计数
	am := s.auth
	if am == nil {
		// 防御：auth 未装配时也要能验证（对库校验），不得退化成免验证
		am = &AuthManager{db: s.db}
	}
	rateKey := clientIP(r) + "|api-key-create"
	if wait := am.checkLoginRate(rateKey); wait > 0 {
		writeAPIError(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(wait.Seconds())+1))
		return
	}
	if !am.verifyPassword(body.VerifyPassword) {
		am.recordLoginFail(rateKey)
		writeAPIError(w, http.StatusUnauthorized, "管理员密码验证失败，请输入当前管理员密码")
		return
	}
	am.clearLoginFail(rateKey)
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 64 {
		writeAPIError(w, http.StatusBadRequest, "name 必填且不超过 64 字符")
		return
	}
	// 名称唯一：用量归因与面板按名字展示 Key，重名会让审计/告知混淆
	if keys, err := s.db.ListGatewayKeys(); err == nil {
		for _, k := range keys {
			if k.Name == name {
				writeAPIError(w, http.StatusConflict, "同名网关 Key 已存在: "+name)
				return
			}
		}
	}
	if body.RPMLimit < 0 || body.RPMLimit > 100000 {
		writeAPIError(w, http.StatusBadRequest, "rpm_limit 取值范围 0-100000（0=不限）")
		return
	}
	if body.QuotaTotal < 0 {
		writeAPIError(w, http.StatusBadRequest, "quota_total 不能为负")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	plain := GenerateAPIKey()
	k := &GatewayKey{
		Name:       name,
		KeyHash:    HashGatewayKey(plain),
		KeyPrefix:  plain[:10] + "…",
		Enabled:    enabled,
		RPMLimit:   body.RPMLimit,
		QuotaTotal: body.QuotaTotal,
		Models:     normalizeModelWhitelist(body.Models),
		Key:        plain,
	}
	id, err := s.db.CreateGatewayKey(k)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	k.ID = id
	log.Printf("[keys] gateway key created: %s (%s…)", name, plain[:10])
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":     k,
		"message": "请立即保存明文 Key，仅此一次展示",
	})
}

func (s *APIServer) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	existing, err := s.db.GetGatewayKey(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "gateway key not found")
		return
	}
	var body struct {
		Name       *string `json:"name"`
		Enabled    *bool   `json:"enabled"`
		RPMLimit   *int    `json:"rpm_limit"`
		QuotaTotal *int64  `json:"quota_total"`
		Models     *string `json:"models"`
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := existing.Name
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 64 {
			writeAPIError(w, http.StatusBadRequest, "name 必填且不超过 64 字符")
			return
		}
		// 名称唯一（改名撞别的 Key 同样拒绝）
		if keys, err := s.db.ListGatewayKeys(); err == nil {
			for _, k := range keys {
				if k.ID != id && k.Name == name {
					writeAPIError(w, http.StatusConflict, "同名网关 Key 已存在: "+name)
					return
				}
			}
		}
	}
	enabled := existing.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rpm := existing.RPMLimit
	if body.RPMLimit != nil {
		if *body.RPMLimit < 0 || *body.RPMLimit > 100000 {
			writeAPIError(w, http.StatusBadRequest, "rpm_limit 取值范围 0-100000（0=不限）")
			return
		}
		rpm = *body.RPMLimit
	}
	quota := existing.QuotaTotal
	if body.QuotaTotal != nil {
		if *body.QuotaTotal < 0 {
			writeAPIError(w, http.StatusBadRequest, "quota_total 不能为负")
			return
		}
		quota = *body.QuotaTotal
	}
	models := existing.Models
	if body.Models != nil {
		models = *body.Models
	}
	if err := s.db.UpdateGatewayKeyFields(id, name, enabled, rpm, quota, models); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	k, err := s.db.GetGatewayKey(id)
	if err != nil {
		// 更新已落库但回读失败：如实报错而不是 200 + key:null
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if k == nil {
		// 保存期间被并发删除：0 行更新不报错，这里如实 404
		writeAPIError(w, http.StatusNotFound, "gateway key was deleted during save")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"key": k})
}

func (s *APIServer) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.db.DeleteGatewayKey(id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// jsonDecodeBody 限制大小的 JSON 请求体解码（管理接口复用）
func jsonDecodeBody(r *http.Request, v interface{}) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
