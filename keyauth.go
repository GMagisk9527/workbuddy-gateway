package main

// 鉴权开关的独立持久化。
//
// 为什么不能只放在 apikeys.json：那个文件是"密钥数据"，一旦被误删、迁移遗漏
// 或写入失败，鉴权就会跟着消失，变成匿名开放。这里用一个小文件记录
// 「这个部署曾经要求过鉴权」，与密钥数据解耦：
//   - 创建过任何受管 Key → 自动置位；
//   - 文件丢失不影响已有 Key（requireAuth 仍是 true）；
//   - apikeys.json 丢了但 keyauth.json 在 → 仍然要求鉴权（不会再退回开放）；
//   - 只有显式把 requireAuth 改成 false（或删除该文件且从未创建过 Key）才是开放。

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const authSwitchName = "keyauth.json"

type authSwitchDoc struct {
	RequireAuth bool  `json:"requireAuth"`
	UpdatedAt   int64 `json:"updatedAt,omitempty"`
}

var (
	authSwitchMu sync.RWMutex
	authSwitchOn bool
)

func authSwitchPath(dir string) string { return filepath.Join(dir, authSwitchName) }

// loadAuthSwitch 读取独立的鉴权开关；文件不存在表示"从未显式要求过"。
func loadAuthSwitch(dir string) error {
	raw, err := os.ReadFile(authSwitchPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		authSwitchMu.Lock()
		authSwitchOn = false
		authSwitchMu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	var doc struct {
		RequireAuth *bool `json:"requireAuth"`
		UpdatedAt   int64 `json:"updatedAt,omitempty"`
	}
	err = json.Unmarshal(raw, &doc)
	if err == nil && doc.RequireAuth == nil {
		err = errors.New("缺少有效的 requireAuth 布尔值")
	}
	if err != nil {
		// 文件损坏时按"要求鉴权"处理：宁可拒绝，也不因读不懂就开放。
		authSwitchMu.Lock()
		authSwitchOn = true
		authSwitchMu.Unlock()
		log.Printf("[APIKey] 警告=鉴权开关文件损坏，已按'要求鉴权'处理 文件=%s 原因=%v", authSwitchName, err)
		return nil
	}
	authSwitchMu.Lock()
	authSwitchOn = *doc.RequireAuth
	authSwitchMu.Unlock()
	return nil
}

// setAuthSwitch 原子写入开关文件。
func setAuthSwitch(dir string, require bool) error {
	authSwitchMu.Lock()
	authSwitchOn = require
	authSwitchMu.Unlock()
	doc := authSwitchDoc{RequireAuth: require, UpdatedAt: time.Now().UnixMilli()}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := authSwitchPath(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func authSwitchEnabled() bool {
	authSwitchMu.RLock()
	defer authSwitchMu.RUnlock()
	return authSwitchOn
}

// ensureAuthSwitchOn 在创建受管 Key 时把开关落盘（失败只记录，不阻断创建）。
func ensureAuthSwitchOn(dir string) {
	if authSwitchEnabled() {
		return
	}
	if err := setAuthSwitch(dir, true); err != nil {
		log.Printf("[APIKey] 警告=鉴权开关写入失败 原因=%v 业务影响=本次仍按内存状态要求鉴权", err)
	}
}
