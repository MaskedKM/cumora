// db —— pg 连接池 + 迁移基线(#51)。
//
// 迁移策略(ADR 0004):drizzle 时代的 schema 由既有 TS 迁移器继续管辖;
// Go 侧 goose 基线 = 一条空基线,后续 Go 域新增的表变更走 goose 增量,
// 与 TS 基线共存不冲突(切换日 #70 后 TS 迁移器退役)。
package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func Open(url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	return db, nil
}

// Migrate 按文件名序执行 migrations/*.sql 中未应用的语句(自管 go_migrations 表;
// 正式 goose 若引入可平滑接管——其默认表名 goose_db_version 不冲突)。
func Migrate(db *sql.DB, dir string) error {
	// 双实例并发靴的 CREATE TABLE IF NOT EXISTS 竞态防护(pg_type 重复键)。
	if _, err := db.Exec(`SELECT pg_advisory_lock(hashtext('cumora_go_migrations'))`); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer db.Exec(`SELECT pg_advisory_unlock(hashtext('cumora_go_migrations'))`)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS go_migrations (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("ensure go_migrations table: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM go_migrations WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO go_migrations (name) VALUES ($1)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slogInfo("migration applied", name)
	}
	return nil
}

func slogInfo(msg, name string) { slog.Info("migration "+msg, "name", name) }
