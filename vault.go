package main

// ---- 凭证静态加密（B3，对齐 zcode-api credentials.json 的静态加密实践）----
//
// SQLite accounts 表中的凭证列（access_token / refresh_token / zcode_jwt /
// api_key / user_info / creds_raw）以 vault1: 信封静态加密存储：
//   "vault1:" + b64url(nonce12) + "." + b64url(tag16) + "." + b64url(ct)
// 密钥 = SHA-256(seed)。seed 解析优先级（ResolveVaultSeed，启动时执行一次）：
//   1. 环境变量 ZCODE_PROXY_VAULT_SECRET
//   2. DB 同目录 vault.key（首次自动生成随机密钥，0600）
//   3. 旧版平台派生种子仅作迁移源：检测到旧密文且可解时重加密至 vault.key
// 读取透明解密；写入透明加密（失败中止写入）；启动时一次性迁移存量明文行（幂等），
// 并做解密健康检查（密文解不开时大声告警）。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const vaultPrefix = "vault1:"

// vaultCredentialColumns 需要静态加密的凭证列
var vaultCredentialColumns = []string{
	"access_token", "refresh_token", "zcode_jwt", "api_key", "user_info", "creds_raw",
}

// vaultSeedOverride 启动时解析出的当前种子（keyfile 或环境变量）；
// 在任何账号读写发生前的 main 启动序列中设置，此后只读。
var (
	vaultSeedMu       sync.RWMutex
	vaultSeedOverride string
	vaultSeedDerived  = true // 启动解析后仍为 true = 只有可推算的派生种子可用（降级态）
)

func setVaultSeedOverride(s string) {
	vaultSeedMu.Lock()
	vaultSeedOverride = s
	vaultSeedDerived = s == "" // 清空覆盖 = 回到派生种子降级态
	vaultSeedMu.Unlock()
}

// vaultSeedIsDerived 当前种子是否为可从平台/用户名推算的派生种子。
// 派生种子仅允许解密与迁移读取，绝不用于加密写入（见 vaultEncrypt）。
func vaultSeedIsDerived() bool {
	vaultSeedMu.RLock()
	defer vaultSeedMu.RUnlock()
	return vaultSeedDerived
}

func currentVaultSeed() string {
	vaultSeedMu.RLock()
	defer vaultSeedMu.RUnlock()
	if vaultSeedOverride != "" {
		return vaultSeedOverride
	}
	return legacyVaultSeed()
}

// vaultKeyFile DB 同目录的密钥文件
func vaultKeyFile(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "vault.key")
}

// randomVaultSeed 生成 32 字节随机种子的 hex 表示
func randomVaultSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loadVaultKeyFile 读取并校验已存在的密钥文件，返回 (seed, ok)
func loadVaultKeyFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(raw))
	if len(s) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return s, true
}

// writeVaultKeyFile 新建密钥文件（0600，fsync 落盘）。仅应在确认文件不存在时调用，
// 绝不覆盖已有密钥——覆盖等于销毁存量密文唯一的钥匙。
// fsync 是必需的：重加密提交依赖 keyfile 先于 WAL 落盘，断电窗口内丢 keyfile
// 等于销毁全部凭证的钥匙。
// 注：Windows 上 Go 的 Chmod 只映射只读位，0600 限制在 win32 不生效（ACL 限制需另做）。
func writeVaultKeyFile(path, seed string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("vault key file %s 已存在，拒绝覆盖", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(seed + "\n")); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// umask 可能放宽权限，显式收紧
	os.Chmod(path, 0600)
	syncDir(filepath.Dir(path)) // 新文件的目录项也要落盘，否则断电可丢文件名
	return nil
}

// syncDir 尽力 fsync 目录项（部分平台/文件系统不支持，失败可忽略）
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// ResolveVaultSeed 启动时决定凭证加密种子，优先级：
//  1. ZCODE_PROXY_VAULT_SECRET 环境变量（显式指定，不读不写密钥文件）
//  2. DB 同目录 vault.key（存在则直接采用；不存在且库中无密文时生成随机密钥）
//  3. 旧版平台派生种子（可从平台/用户名推算，仅作为一次性迁移源：
//     库中有密文、派生种子能解开全部密文且无 keyfile 时，生成 keyfile 并重加密）
//
// 库中存在密文但当前候选钥匙解不开时，保持派生种子并大声告警，绝不换钥匙——
// 换钥匙会让存量密文永久不可解。重加密失败时回滚种子并删除刚建的 keyfile，
// 避免下次启动采用一把和库中数据不匹配的钥匙。
func ResolveVaultSeed(db *DB, dbPath string) {
	if s := os.Getenv("ZCODE_PROXY_VAULT_SECRET"); s != "" {
		setVaultSeedOverride(s)
		return
	}
	keyFile := vaultKeyFile(dbPath)
	legacy := legacyVaultSeed()

	// 1. 已有 keyfile：先按其种子全量验证再采用
	if keyFile != "" {
		if seed, ok := loadVaultKeyFile(keyFile); ok {
			total, broken, scanErr := db.scanVaultCiphertext(seed)
			if scanErr != nil {
				// 盘点失败（I/O）不得当作"无密文/全可解"处理：fail closed，留在派生种子
				log.Printf("[vault] WARNING: ciphertext scan failed (%v); staying on derived key", scanErr)
				return
			}
			if broken == 0 {
				// keyfile 与全部密文匹配：常规重启路径，直接采用
				setVaultSeedOverride(seed)
				return
			}
			// 按 keyfile 自身的可解比例分派（不以派生种子扫描结果为主判据——
			// 否则"keyfile 行 + 卡死行"混存时会因派生种子全解不开而误判为
			// 真钥匙不匹配，降级态自我维持、永不收敛）：
			//   broken == 0        → 常规重启，直接采用（上方分支）；
			//   0 < broken < total → keyfile 能解开部分数据，是（部分）正确的主钥匙：
			//                        并入派生种子可解的行后采用（对卡死行是 no-op），
			//                        混合钥匙库与"keyfile+卡死行"两种状态在此收敛；
			//   broken == total    → keyfile 一条都解不开：
			//                        派生种子全可解 → 崩溃轮换的孤儿 keyfile，删除重轮换；
			//                        派生种子部分/全部不可解 → 真钥匙不匹配
			//                        （曾用 env / 异机库），留在派生种子并告警。
			if broken < total {
				moved, stuck, cerr := db.consolidateMixedVaultRows(seed, legacy)
				if cerr != nil {
					log.Printf("[vault] WARNING: mixed-key consolidation failed (%v); staying on derived key", cerr)
					return
				}
				log.Printf("[vault] consolidated %d mixed-key credential values under vault.key (%d left unreadable)", moved, stuck)
				// keyfile 是唯一能解开部分数据的钥匙，采用它；仍解不开的值
				// （env 期写入/损坏）只能等原钥匙恢复
				setVaultSeedOverride(seed)
				if t2, b2, e2 := db.scanVaultCiphertext(seed); e2 == nil && b2 > 0 {
					log.Printf("[vault] WARNING: %d of %d values still need their original key (set ZCODE_PROXY_VAULT_SECRET) — "+
						"those accounts stay credential-less until restored", b2, t2)
				}
				return
			}
			// broken == total：keyfile 解不开任何数据
			lTotal, lBroken, lErr := db.scanVaultCiphertext(legacy)
			if lErr != nil {
				log.Printf("[vault] WARNING: derived-key scan failed (%v); staying on derived key", lErr)
				return
			}
			if lTotal > 0 && lBroken == 0 {
				if rmErr := os.Remove(keyFile); rmErr != nil {
					log.Printf("[vault] WARNING: failed to remove orphan key file %s (%v); rotation is blocked until it is removed manually", keyFile, rmErr)
					return
				}
				syncDir(filepath.Dir(keyFile))
				log.Printf("[vault] removed orphan key file from an interrupted rotation; re-running rotation")
			} else {
				log.Printf("[vault] WARNING: vault.key cannot decrypt any of %d encrypted credential values, and the derived key "+
					"recovers %d of %d. If ZCODE_PROXY_VAULT_SECRET was previously set, run with it again; "+
					"staying on the derived key — credential WRITES ARE REFUSED in this state until the matching key is restored.",
					total, lTotal-lBroken, lTotal)
				return
			}
		} else if _, statErr := os.Stat(keyFile); statErr == nil {
			log.Printf("[vault] WARNING: key file %s 存在但格式非法，忽略（如需重置请手动删除）", keyFile)
		}
	}

	// 2. 全量盘点库中密文在派生种子下的可解性
	total, broken, scanErr := db.scanVaultCiphertext(legacy)
	if scanErr != nil {
		log.Printf("[vault] WARNING: ciphertext scan failed (%v); staying on derived key", scanErr)
		return
	}
	if total == 0 {
		// 无存量密文：生成随机 keyfile 并启用
		if keyFile == "" {
			return // 无法定位 keyfile（dbPath 为空）：留在派生种子
		}
		seed, err := randomVaultSeed()
		if err == nil {
			err = writeVaultKeyFile(keyFile, seed)
			if err == nil {
				log.Printf("[vault] generated random vault key: %s", keyFile)
				setVaultSeedOverride(seed)
				return
			}
		}
		// 生成/落盘失败：进入派生种子降级态——凭证加密写入将被拒绝，
		// 恢复数据目录权限后重启即可重新生成
		log.Printf("[vault] WARNING: key file unavailable (%v); entering degraded derived-key state — "+
			"credential writes are refused until data-dir permissions are fixed and the proxy restarted", errOr(err))
		return
	}
	if broken > 0 {
		// 有密文但派生种子解不开：曾用 env 加密 / DB 换机。绝不盲目换新钥匙。
		log.Printf("[vault] WARNING: %d of %d encrypted credential values cannot be decrypted with the derived key. "+
			"If ZCODE_PROXY_VAULT_SECRET was previously set, run with it again; "+
			"if the database was moved from another machine/user, restore its vault.key. "+
			"Affected accounts will appear credential-less until the key is restored.", broken, total)
		return
	}
	// 3. 派生种子可解开全部密文：一次性轮换到随机 keyfile
	if keyFile == "" {
		return
	}
	newSeed, err := randomVaultSeed()
	if err != nil {
		log.Printf("[vault] generate new key: %v; staying on derived key", err)
		return
	}
	if err := writeVaultKeyFile(keyFile, newSeed); err != nil {
		log.Printf("[vault] write key file: %v; staying on derived key", err)
		return
	}
	if _, err := db.reencryptVaultColumns(legacy, newSeed); err != nil {
		// 回滚：恢复派生种子并删掉刚建的 keyfile，否则下次启动会采用
		// 一把和库中数据不匹配的钥匙
		if rmErr := os.Remove(keyFile); rmErr != nil {
			log.Printf("[vault] WARNING: rotation failed (%v) AND removing %s failed (%v); delete it manually or next start will adopt a mismatched key", err, keyFile, rmErr)
			return
		}
		syncDir(filepath.Dir(keyFile))
		log.Printf("[vault] key rotation failed (%v); rolled back to derived key and removed %s", err, keyFile)
		return
	}
	setVaultSeedOverride(newSeed)
	log.Printf("[vault] rotated credential values from derived key to random key file %s", keyFile)
}

func errOr(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// scanVaultCiphertext 全量扫描库中 vault1 密文，返回 (总数, 在给定种子下解密失败的条数, 错误)。
// 轮换决策必须全量：抽样会漏掉个别损坏/异源行，导致重加密中途失败。
// 查询/迭代失败必须上抛：调用方把"扫不到"当"没有"会基于错误前提换钥匙。
func (db *DB) scanVaultCiphertext(seed string) (total, broken int, err error) {
	for _, col := range vaultCredentialColumns {
		rows, err := db.conn.Query(`SELECT ` + col + ` FROM accounts WHERE ` + col + ` LIKE '` + vaultPrefix + `%'`)
		if err != nil {
			return total, broken, fmt.Errorf("scan %s: %w", col, err)
		}
		for rows.Next() {
			var val string
			if rows.Scan(&val) != nil {
				// 扫不动（NULL/异型值）按 broken 计：否则轮换会在 reencrypt 阶段
				// 撞上同一行才发现问题
				total++
				broken++
				continue
			}
			if !strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			total++
			if _, err := DecryptCredential(encPrefix+strings.TrimPrefix(val, vaultPrefix), seed); err != nil {
				broken++
			}
		}
		if rErr := rows.Err(); rErr != nil {
			rows.Close()
			return total, broken, fmt.Errorf("scan %s rows: %w", col, rErr)
		}
		rows.Close()
	}
	return total, broken, nil
}

// consolidateMixedVaultRows 逐行双种子解密，把 keySeed 解不开而 legacySeed 能解开的
// 凭证值统一重加密到 keySeed（单事务）。返回 (迁移值数, 两种种子都解不开的值数)——
// 计数按"值"而非"行"（同一行的多列可能分别落在两把钥匙下）。
// 与 reencryptVaultColumns 的全量重加密不同：只动需要动的行，解不开的行保持原样。
func (db *DB) consolidateMixedVaultRows(keySeed, legacySeed string) (int, int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	moved, stuck := 0, 0
	for _, col := range vaultCredentialColumns {
		rows, err := tx.Query(`SELECT id, ` + col + ` FROM accounts WHERE ` + col + ` LIKE '` + vaultPrefix + `%'`)
		if err != nil {
			return moved, stuck, fmt.Errorf("consolidate query %s: %w", col, err)
		}
		type pending struct {
			id  int64
			enc string
		}
		var updates []pending
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				// NULL/异型值（外部损坏、部分恢复的库）按 stuck 语义跳过并保留
				// 原样——中止整个事务会把一把已证明可解部分数据的钥匙挡在门外
				stuck++
				continue
			}
			if !strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			body := encPrefix + strings.TrimPrefix(val, vaultPrefix)
			if _, kerr := DecryptCredential(body, keySeed); kerr == nil {
				continue // 已在 keyfile 种子下
			}
			plain, lerr := DecryptCredential(body, legacySeed)
			if lerr != nil {
				stuck++ // 两种种子都解不开（env 期写入等）：保持原样等钥匙恢复
				continue
			}
			enc, err := EncryptCredential(plain, keySeed)
			if err != nil {
				rows.Close()
				return moved, stuck, fmt.Errorf("consolidate encrypt %s id=%d: %w", col, id, err)
			}
			updates = append(updates, pending{id: id, enc: vaultPrefix + strings.TrimPrefix(enc, encPrefix)})
		}
		rows.Close()
		if rErr := rows.Err(); rErr != nil {
			return moved, stuck, rErr
		}
		for _, up := range updates {
			if _, err := tx.Exec(`UPDATE accounts SET `+col+` = ? WHERE id = ?`, up.enc, up.id); err != nil {
				return moved, stuck, fmt.Errorf("consolidate update %s id=%d: %w", col, up.id, err)
			}
		}
		moved += len(updates)
	}
	return moved, stuck, tx.Commit()
}

// reencryptVaultColumns 用 oldSeed 解密、newSeed 重加密所有 vault1 凭证列。
// 单事务执行：任一行失败整体回滚，库里不会出现两种钥匙混存的半迁移状态。
func (db *DB) reencryptVaultColumns(oldSeed, newSeed string) (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	moved := 0
	for _, col := range vaultCredentialColumns {
		rows, err := tx.Query(`SELECT id, ` + col + ` FROM accounts WHERE ` + col + ` LIKE '` + vaultPrefix + `%'`)
		if err != nil {
			return 0, fmt.Errorf("reencrypt query %s: %w", col, err)
		}
		type pending struct {
			id  int64
			enc string
		}
		var updates []pending
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return 0, err
			}
			if !strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			plain, err := DecryptCredential(encPrefix+strings.TrimPrefix(val, vaultPrefix), oldSeed)
			if err != nil {
				rows.Close()
				return 0, fmt.Errorf("reencrypt decrypt %s id=%d: %w", col, id, err)
			}
			enc, err := EncryptCredential(plain, newSeed)
			if err != nil {
				rows.Close()
				return 0, fmt.Errorf("reencrypt encrypt %s id=%d: %w", col, id, err)
			}
			updates = append(updates, pending{id: id, enc: vaultPrefix + strings.TrimPrefix(enc, encPrefix)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for _, u := range updates {
			if _, err := tx.Exec(`UPDATE accounts SET `+col+` = ? WHERE id = ?`, u.enc, u.id); err != nil {
				return 0, fmt.Errorf("reencrypt update %s id=%d: %w", col, u.id, err)
			}
		}
		moved += len(updates)
	}
	return moved, tx.Commit()
}

// ProbeVaultHealth 启动收尾的健康检查：统计当前种子解不开的密文数量并大声告警
func (db *DB) ProbeVaultHealth() {
	_, broken, err := db.scanVaultCiphertext(currentVaultSeed())
	if err != nil {
		log.Printf("[vault] WARNING: health probe failed: %v", err)
		return
	}
	if broken > 0 {
		log.Printf("[vault] WARNING: %d credential values cannot be decrypted with the active key "+
			"(set ZCODE_PROXY_VAULT_SECRET or restore data/vault.key); affected accounts will fail auth until fixed", broken)
	}
	// 设置表机密项（网关 sk- 密钥、口令哈希）不在 scanVaultCiphertext 覆盖内，单独体检
	for _, key := range vaultSecretSettings {
		var val string
		if err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val); err != nil || val == "" || !strings.HasPrefix(val, vaultPrefix) {
			continue
		}
		if _, err := DecryptCredential(encPrefix+strings.TrimPrefix(val, vaultPrefix), currentVaultSeed()); err != nil {
			log.Printf("[vault] WARNING: setting %s cannot be decrypted with the active key "+
				"(wrong ZCODE_PROXY_VAULT_SECRET or mismatched vault.key); gateway auth and admin login may fail", key)
		}
	}
}

// VaultSecret 返回当前凭证加密种子
func VaultSecret() string {
	return currentVaultSeed()
}

// legacyVaultSeed 旧版派生种子（可由平台/home/用户名推算，仅作迁移源）
func legacyVaultSeed() string {
	home, _ := os.UserHomeDir()
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" {
		username = "unknown"
	}
	return fmt.Sprintf("zcode-proxy-vault:%s:%s:%s", NodePlatform(), home, username)
}

// vaultEncrypt 加密单值；失败必须由调用方中止写入——静默落明文会让
// "加密存储"在加密层故障时变成明文存储且无人察觉。
// 派生种子降级态下拒绝加密新明文：派生种子可从平台/用户名推算，
// 用它加密等于明文等价保护。已是本种子可解密文的原值透传（幂等再写不产生新弱点）。
func vaultEncrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if strings.HasPrefix(plain, vaultPrefix) {
		// 已是密文形态：仅当确实能解开才透传（幂等再写）；解不开的
		// "vault1:" 串是脏数据，写入库只会永久占位 broken 并在读取时变空
		if _, err := DecryptCredential(encPrefix+strings.TrimPrefix(plain, vaultPrefix), VaultSecret()); err == nil {
			return plain, nil
		}
		return "", fmt.Errorf("值带 vault1: 前缀但不是可解密文，拒绝写入")
	}
	if vaultSeedIsDerived() {
		return "", fmt.Errorf("vault 处于降级状态（仅剩可推算的派生密钥）：拒绝加密写入新凭证 — " +
			"恢复 data/vault.key 或设置 ZCODE_PROXY_VAULT_SECRET 后重启")
	}
	enc, err := EncryptCredential(plain, VaultSecret())
	if err != nil {
		return "", fmt.Errorf("vault encrypt: %w", err)
	}
	return vaultPrefix + strings.TrimPrefix(enc, encPrefix), nil
}

func vaultDecrypt(value string) string {
	if value == "" || !strings.HasPrefix(value, vaultPrefix) {
		return value
	}
	// vault1: 与 enc:v1 信封布局一致，仅前缀与密钥不同
	out, err := DecryptCredential(encPrefix+strings.TrimPrefix(value, vaultPrefix), VaultSecret())
	if err != nil {
		log.Printf("[vault] decrypt failed: %v", err)
		return ""
	}
	return out
}

// MigrateVault 把存量明文凭证列加密（幂等：已加密值跳过）。启动时调用一次。
// 派生种子降级态下跳过：用可推算密钥"升级"明文只是伪装的保护，等待真钥匙恢复。
func (db *DB) MigrateVault() error {
	if vaultSeedIsDerived() {
		log.Printf("[vault] 派生密钥降级态：跳过明文迁移（避免用可推算密钥加密存量明文）；恢复钥匙后重启将自动迁移")
		return nil
	}
	secret := VaultSecret()
	hadUpdates := false
	for _, col := range vaultCredentialColumns {
		rows, err := db.conn.Query(`SELECT id, ` + col + ` FROM accounts WHERE ` + col + ` != ''`)
		if err != nil {
			return fmt.Errorf("vault migrate query %s: %w", col, err)
		}
		type pending struct {
			id  int64
			enc string
		}
		var updates []pending
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return err
			}
			if val == "" || strings.HasPrefix(val, vaultPrefix) {
				continue
			}
			enc, err := EncryptCredential(val, secret)
			if err != nil {
				rows.Close()
				return fmt.Errorf("vault encrypt %s id=%d: %w", col, id, err)
			}
			updates = append(updates, pending{id: id, enc: vaultPrefix + strings.TrimPrefix(enc, encPrefix)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := db.conn.Exec(`UPDATE accounts SET `+col+` = ? WHERE id = ?`, u.enc, u.id); err != nil {
				return fmt.Errorf("vault update %s id=%d: %w", col, u.id, err)
			}
		}
		if len(updates) > 0 {
			log.Printf("[vault] encrypted %d plaintext values in column %s", len(updates), col)
			hadUpdates = true
		}
	}
	// 设置表机密项（网关 sk- 密钥、bcrypt 口令哈希）一并入库加密
	for _, key := range vaultSecretSettings {
		var val string
		err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
		if err != nil || val == "" || strings.HasPrefix(val, vaultPrefix) {
			continue
		}
		enc, err := EncryptCredential(val, secret)
		if err != nil {
			return fmt.Errorf("vault encrypt setting %s: %w", key, err)
		}
		if _, err := db.conn.Exec(`UPDATE settings SET value = ? WHERE key = ?`, vaultPrefix+strings.TrimPrefix(enc, encPrefix), key); err != nil {
			return fmt.Errorf("vault update setting %s: %w", key, err)
		}
		hadUpdates = true
		log.Printf("[vault] encrypted setting %s", key)
	}
	if hadUpdates {
		// 迁移前的明文会残留在 WAL 与空闲页：checkpoint 截断 WAL，VACUUM 重写并清空 freelist
		db.conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		db.conn.Exec(`VACUUM`)
		log.Printf("[vault] post-migration WAL checkpoint + VACUUM done (plaintext residue scrubbed)")
	}
	return nil
}

// vaultSecretSettings 需要静态加密的设置键
var vaultSecretSettings = []string{"api_key", "password_hash"}
