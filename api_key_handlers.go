package main

import (
	"encoding/json"
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
		Name       string `json:"name"`
		RPMLimit   int    `json:"rpm_limit"`
		QuotaTotal int64  `json:"quota_total"`
		Models     string `json:"models"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 64 {
		writeAPIError(w, http.StatusBadRequest, "name 必填且不超过 64 字符")
		return
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
