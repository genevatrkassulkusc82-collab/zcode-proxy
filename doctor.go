package main

import (
	"database/sql"
	"fmt"
	"os"
)

// ---- 离线体检（-doctor）----
// 不打任何上游请求：配置 / 数据库完整性 / vault 加解密 / 账号状态 /
// 出口代理 / 网关 Key / 用量记录。退出码 0=全部通过，1=存在 FAIL。

func runDoctor(cfg *FileConfig, db *DB) int {
	fail := 0
	ok := func(name, detail string) {
		fmt.Printf("[ OK ] %s: %s\n", name, detail)
	}
	warn := func(name, detail string) {
		fmt.Printf("[WARN] %s: %s\n", name, detail)
	}
	bad := func(name, detail string) {
		fail++
		fmt.Printf("[FAIL] %s: %s\n", name, detail)
	}

	// 1. 配置
	models := cfg.GetModels()
	if len(models) == 0 {
		warn("config", "models 为空（将仅放行未知模型直通）")
	} else {
		ok("config", fmt.Sprintf("dir=%s models=%d (首个: %s)", cfg.ConfigDir(), len(models), models[0]))
	}

	// 2. SQLite 完整性
	if err := db.QuickCheck(); err != nil {
		bad("database", "quick_check: "+err.Error())
	} else {
		ok("database", "PRAGMA quick_check 通过")
	}

	// 3. vault 加解密回环
	probe := "doctor-probe-炊烟"
	if enc, err := vaultEncrypt(probe); err != nil {
		bad("vault", "加密失败: "+err.Error())
	} else if got := vaultDecrypt(enc); got != probe {
		bad("vault", "回环失配（密钥不可用？）")
	} else {
		ok("vault", "AES-256-GCM 加解密回环通过")
	}

	// 3.5 验证码求解浏览器（离线 stat，不打上游）
	if bin := findRealBrowser(); bin == "" {
		warn("captcha_browser", "未找到本机真实 Chrome/Edge（将回退 rod 托管 Chromium，易被阿里云风控识别；有头手动档不可用）")
	} else {
		ok("captcha_browser", bin)
	}

	// 4. 账号状态
	if counts, err := db.CountAccountsByStatus(); err != nil {
		bad("accounts", err.Error())
	} else {
		total := 0
		for _, n := range counts {
			total += n
		}
		if total == 0 {
			warn("accounts", "库内无账号（先在 Web 面板导入）")
		} else {
			detail := fmt.Sprintf("total=%d", total)
			for _, st := range []string{StatusActive, StatusCooling, StatusExhausted, StatusInvalid, StatusDisabled, StatusInactive} {
				if n := counts[st]; n > 0 {
					detail += fmt.Sprintf(" %s=%d", st, n)
				}
			}
			if counts[StatusInvalid] > 0 {
				warn("accounts", detail+"（invalid 需人工处理）")
			} else {
				ok("accounts", detail)
			}
		}
	}

	// 5. 出口代理
	if nodes, err := db.ListProxyNodes(); err != nil {
		bad("proxies", err.Error())
	} else {
		enabled, hasDefault := 0, false
		for _, n := range nodes {
			if n.Enabled {
				enabled++
				hasDefault = hasDefault || n.IsDefault
			}
		}
		if enabled == 0 {
			ok("proxies", "无启用节点（直连出网）")
		} else if !hasDefault {
			warn("proxies", fmt.Sprintf("%d 个启用节点但未设默认（未绑组的账号将直连）", enabled))
		} else {
			ok("proxies", fmt.Sprintf("enabled=%d default=已设置", enabled))
		}
	}

	// 6. 网关 Key（R1）
	if keys, err := db.ListGatewayKeys(); err != nil {
		bad("gateway_keys", err.Error())
	} else {
		disabled := 0
		for _, k := range keys {
			if !k.Enabled {
				disabled++
			}
		}
		ok("gateway_keys", fmt.Sprintf("total=%d disabled=%d（根 api_key 不受影响）", len(keys), disabled))
	}

	// 7. 用量记录
	var usage int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&usage); err != nil && err != sql.ErrNoRows {
		bad("usage_records", err.Error())
	} else {
		ok("usage_records", fmt.Sprintf("%d 条", usage))
	}

	fmt.Println("----------------------------------------")
	if fail > 0 {
		fmt.Printf("doctor: %d 项 FAIL\n", fail)
		return 1
	}
	fmt.Println("doctor: 全部通过")
	return 0
}

// QuickCheck SQLite 快速完整性检查
func (db *DB) QuickCheck() error {
	var result string
	row := db.conn.QueryRow(`PRAGMA quick_check`)
	if err := row.Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check = %q", result)
	}
	return nil
}

// runDoctorAndExit 供 main -doctor 调用：输出报告并以相应码退出
func runDoctorAndExit(cfg *FileConfig, db *DB) {
	os.Exit(runDoctor(cfg, db))
}
