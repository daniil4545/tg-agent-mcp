// Package db - открытие пула PostgreSQL и транзакция.
//
// Всё остальное в слое БД у сервисов своё: запросы и разбор строк идут вместе с
// доменом, и общего в них нет ничего, кроме этих двух функций.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open открывает пул и сразу проверяет связь. Пинг на старте обязателен: без
// него сервис поднимается «здоровым» и падает на первом же запросе, когда
// разбираться уже некогда.
//
// maxConns берётся не с потолка: он обязан покрывать все горутины, которые
// одновременно держат соединение - воркеры очереди, фоновые опросы, обработчики
// входящих. Меньше - и работы встанут в очередь за соединением, а выглядеть это
// будет как медленный внешний сервис.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// InTx выполняет fn в транзакции. Откат отложен и срабатывает на любом выходе,
// включая панику; после успешного Commit он уже безвреден.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
