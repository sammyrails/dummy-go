package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tracelit "github.com/tracelit-ai/tracelit-go"
)

// ErrNotFound is returned when a product cannot be found.
var ErrNotFound = errors.New("product not found")

const dbSystem = "postgresql"

// dbSpan starts a client span for a DB operation with standard semantic
// convention attributes pre-populated.
func dbSpan(ctx context.Context, op, table, stmt string) (context.Context, *tracelit.Span) {
	return tracelit.StartClientSpan(ctx, "db."+op,
		tracelit.WithSpanAttributes(map[string]any{
			"db.system":    dbSystem,
			"db.operation": op,
			"db.sql.table": table,
			"db.statement": stmt,
		}),
	)
}

// runMigrations creates the products table if it doesn't exist.
func runMigrations(ctx context.Context, db *sql.DB) error {
	const stmt = `CREATE TABLE IF NOT EXISTS products (
		id          BIGSERIAL PRIMARY KEY,
		name        VARCHAR(255) NOT NULL,
		description TEXT         NOT NULL DEFAULT '',
		price       NUMERIC(12, 2) NOT NULL CHECK (price >= 0),
		stock       INTEGER       NOT NULL DEFAULT 0 CHECK (stock >= 0),
		created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`
	ctx, span := dbSpan(ctx, "CREATE TABLE IF NOT EXISTS", "products", stmt)
	defer span.End()

	_, err := db.ExecContext(ctx, stmt)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("create products table: %w", err)
	}
	return nil
}

func createProduct(ctx context.Context, db *sql.DB, req CreateProductRequest) (Product, error) {
	const stmt = `INSERT INTO products (name, description, price, stock)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, description, price, stock, created_at, updated_at`

	ctx, span := dbSpan(ctx, "INSERT", "products", stmt)
	defer span.End()

	var p Product
	err := db.QueryRowContext(ctx, stmt, req.Name, req.Description, req.Price, req.Stock).Scan(
		&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		span.RecordError(err)
		return Product{}, fmt.Errorf("insert product: %w", err)
	}

	span.SetAttributes(map[string]any{
		"db.rows_affected": 1,
		"product.id":       p.ID,
	})
	slog.InfoContext(ctx, "product created", "product_id", p.ID, "name", p.Name)
	return p, nil
}

func getProduct(ctx context.Context, db *sql.DB, id int64) (Product, error) {
	const stmt = `SELECT id, name, description, price, stock, created_at, updated_at
		FROM products WHERE id = $1`

	ctx, span := dbSpan(ctx, "SELECT", "products", stmt)
	defer span.End()
	span.SetAttribute("product.id", id)

	var p Product
	err := db.QueryRowContext(ctx, stmt, id).Scan(
		&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		span.SetAttribute("db.rows_found", 0)
		return Product{}, ErrNotFound
	}
	if err != nil {
		span.RecordError(err)
		return Product{}, fmt.Errorf("query product %d: %w", id, err)
	}

	span.SetAttribute("db.rows_found", 1)
	return p, nil
}

func listProducts(ctx context.Context, db *sql.DB, limit, offset int) ([]Product, int, error) {
	// Count span
	ctx, countSpan := dbSpan(ctx, "SELECT COUNT", "products", "SELECT COUNT(*) FROM products")
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM products").Scan(&total); err != nil {
		countSpan.RecordError(err)
		countSpan.End()
		return nil, 0, fmt.Errorf("count products: %w", err)
	}
	countSpan.SetAttribute("db.count_result", total)
	countSpan.End()

	// List span
	const listStmt = `SELECT id, name, description, price, stock, created_at, updated_at
		FROM products ORDER BY id DESC LIMIT $1 OFFSET $2`
	ctx, listSpan := dbSpan(ctx, "SELECT", "products", listStmt)
	defer listSpan.End()
	listSpan.SetAttributes(map[string]any{
		"query.limit":  limit,
		"query.offset": offset,
	})

	rows, err := db.QueryContext(ctx, listStmt, limit, offset)
	if err != nil {
		listSpan.RecordError(err)
		return nil, 0, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.CreatedAt, &p.UpdatedAt); err != nil {
			listSpan.RecordError(err)
			return nil, 0, fmt.Errorf("scan product row: %w", err)
		}
		products = append(products, p)
	}
	listSpan.SetAttribute("db.rows_found", len(products))
	return products, total, rows.Err()
}

func updateProduct(ctx context.Context, db *sql.DB, id int64, req UpdateProductRequest) (Product, error) {
	const stmt = `UPDATE products SET
		name        = COALESCE($2, name),
		description = COALESCE($3, description),
		price       = COALESCE($4, price),
		stock       = COALESCE($5, stock),
		updated_at  = $6
		WHERE id = $1`

	ctx, span := dbSpan(ctx, "UPDATE", "products", stmt)
	defer span.End()
	span.SetAttribute("product.id", id)

	result, err := db.ExecContext(ctx, stmt,
		id, req.Name, req.Description, req.Price, req.Stock, time.Now())
	if err != nil {
		span.RecordError(err)
		return Product{}, fmt.Errorf("update product %d: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		span.RecordError(err)
		return Product{}, fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		span.SetAttribute("db.rows_affected", 0)
		return Product{}, ErrNotFound
	}

	span.SetAttribute("db.rows_affected", rows)
	return getProduct(ctx, db, id)
}

func deleteProduct(ctx context.Context, db *sql.DB, id int64) error {
	const stmt = "DELETE FROM products WHERE id = $1"

	ctx, span := dbSpan(ctx, "DELETE", "products", stmt)
	defer span.End()
	span.SetAttribute("product.id", id)

	result, err := db.ExecContext(ctx, stmt, id)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("delete product %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		span.SetAttribute("db.rows_affected", 0)
		return ErrNotFound
	}

	span.SetAttribute("db.rows_affected", rows)
	slog.InfoContext(ctx, "product deleted", "product_id", id)
	return nil
}

// slowSearch simulates an expensive full-table scan — used to demo latency traces.
func slowSearch(ctx context.Context, db *sql.DB, query string) ([]Product, error) {
	const stmt = `SELECT id, name, description, price, stock, created_at, updated_at
		FROM products, pg_sleep(1)
		WHERE name ILIKE $1 OR description ILIKE $1
		LIMIT 50`

	ctx, span := dbSpan(ctx, "SELECT", "products", stmt)
	defer span.End()
	span.SetAttributes(map[string]any{
		"search.query":    query,
		"search.strategy": "full_table_scan",
		"query.limit":     50,
	})

	rows, err := db.QueryContext(ctx, stmt, "%"+query+"%")
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("slow search: %w", err)
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.CreatedAt, &p.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan row: %w", err)
		}
		products = append(products, p)
	}
	span.SetAttribute("db.rows_found", len(products))
	return products, rows.Err()
}
