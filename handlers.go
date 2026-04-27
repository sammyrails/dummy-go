package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	tracelit "github.com/tracelit/tracelit-go"
	tlmiddleware "github.com/tracelit/tracelit-go/middleware"
)

// server groups the dependencies shared across all handlers.
type server struct {
	db *sql.DB
}

func (s *server) registerRoutes(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, tlmiddleware.PatternMiddleware(h))
	}

	handle("GET /health", s.handleHealth)

	// Happy-path CRUD
	handle("POST /products", s.handleCreateProduct)
	handle("GET /products", s.handleListProducts)
	handle("GET /products/{id}", s.handleGetProduct)
	handle("PUT /products/{id}", s.handleUpdateProduct)
	handle("DELETE /products/{id}", s.handleDeleteProduct)

	// Slow query demo (shows latency in traces)
	handle("GET /products/search", s.handleSearchProducts)

	// Intentional error paths — used to showcase SDK error capture
	handle("GET /error/panic", s.handleErrorPanic)
	handle("GET /error/notfound", s.handleErrorNotFound)
	handle("GET /error/db", s.handleErrorDB)
	handle("GET /error/validation", s.handleErrorValidation)
	handle("GET /error/timeout", s.handleErrorTimeout)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{
		Error:   msg,
		Code:    code,
		TraceID: tracelit.TraceID(r.Context()),
	})
}

func parseID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id %q: must be a positive integer", raw)
	}
	return id, nil
}

// ── CRUD handlers ─────────────────────────────────────────────────────────────
// The Tracelit HTTP middleware (tlmiddleware.NewHTTPHandler) automatically
// creates a server span for every request, sets http.method, http.route,
// http.status_code, and records request/response sizes as metrics.
// PatternMiddleware (applied per-route above) fills in the route template.
//
// Inside handlers we only need to:
//   - Add domain-specific span attributes with span.SetAttribute(s)
//   - Call span.RecordError(err) so errors surface in the Errors view

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	if err := s.db.PingContext(ctx); err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "health check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy", "error": err.Error(),
		})
		return
	}

	slog.DebugContext(ctx, "health check passed")
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok", "service": "products-crud-api",
	})
}

func (s *server) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	var req CreateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		slog.WarnContext(ctx, "invalid request body", "error", err)
		writeError(w, r, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON: "+err.Error())
		return
	}

	span.SetAttributes(map[string]any{
		"product.name":  req.Name,
		"product.price": req.Price,
		"product.stock": req.Stock,
	})

	if req.Name == "" {
		writeError(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "name is required")
		return
	}
	if req.Price < 0 {
		writeError(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "price must be >= 0")
		return
	}

	product, err := createProduct(ctx, s.db, req)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "failed to create product", "product_name", req.Name, "error", err)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "failed to create product")
		return
	}

	span.SetAttribute("product.id", product.ID)
	slog.InfoContext(ctx, "product created",
		"product_id", product.ID,
		"product_name", product.Name,
		"product_price", product.Price,
	)
	writeJSON(w, http.StatusCreated, product)
}

func (s *server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	limit, offset := 20, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	span.SetAttributes(map[string]any{
		"query.limit":  limit,
		"query.offset": offset,
	})

	products, total, err := listProducts(ctx, s.db, limit, offset)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "failed to list products", "limit", limit, "offset", offset, "error", err)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "failed to retrieve products")
		return
	}

	if products == nil {
		products = []Product{}
	}

	span.SetAttributes(map[string]any{
		"result.count": len(products),
		"result.total": total,
	})
	slog.InfoContext(ctx, "products listed", "count", len(products), "total", total)
	writeJSON(w, http.StatusOK, ListProductsResponse{Data: products, Total: total})
}

func (s *server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	id, err := parseID(r)
	if err != nil {
		span.RecordError(err)
		slog.WarnContext(ctx, "invalid product id", "raw_id", r.PathValue("id"), "error", err)
		writeError(w, r, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	span.SetAttribute("product.id", id)

	product, err := getProduct(ctx, s.db, id)
	if errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "product not found", "product_id", id)
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("product %d not found", id))
		return
	}
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "failed to get product", "product_id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "failed to retrieve product")
		return
	}

	span.SetAttributes(map[string]any{
		"product.name":  product.Name,
		"product.price": product.Price,
		"product.stock": product.Stock,
	})
	slog.InfoContext(ctx, "product retrieved", "product_id", id, "product_name", product.Name)
	writeJSON(w, http.StatusOK, product)
}

func (s *server) handleUpdateProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	id, err := parseID(r)
	if err != nil {
		span.RecordError(err)
		writeError(w, r, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	span.SetAttribute("product.id", id)

	var req UpdateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		slog.WarnContext(ctx, "invalid update body", "product_id", id, "error", err)
		writeError(w, r, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON: "+err.Error())
		return
	}

	if req.Name != nil {
		span.SetAttribute("update.name", *req.Name)
	}
	if req.Price != nil {
		span.SetAttribute("update.price", *req.Price)
	}

	product, err := updateProduct(ctx, s.db, id, req)
	if errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "product not found for update", "product_id", id)
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("product %d not found", id))
		return
	}
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "failed to update product", "product_id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "failed to update product")
		return
	}

	slog.InfoContext(ctx, "product updated",
		"product_id", id,
		"product_name", product.Name,
		"product_price", product.Price,
	)
	writeJSON(w, http.StatusOK, product)
}

func (s *server) handleDeleteProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	id, err := parseID(r)
	if err != nil {
		span.RecordError(err)
		writeError(w, r, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	span.SetAttribute("product.id", id)

	if err := deleteProduct(ctx, s.db, id); errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "product not found for delete", "product_id", id)
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("product %d not found", id))
		return
	} else if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "failed to delete product", "product_id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "failed to delete product")
		return
	}

	slog.InfoContext(ctx, "product deleted", "product_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSearchProducts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)

	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, r, http.StatusBadRequest, "MISSING_QUERY", "query parameter 'q' is required")
		return
	}

	span.SetAttribute("search.query", q)
	slog.InfoContext(ctx, "product search started", "query", q)

	products, err := slowSearch(ctx, s.db, q)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "slow search failed", "query", q, "error", err)
		writeError(w, r, http.StatusInternalServerError, "SEARCH_ERROR", "search failed")
		return
	}

	if products == nil {
		products = []Product{}
	}

	span.SetAttribute("search.result_count", len(products))
	slog.InfoContext(ctx, "product search completed", "query", q, "results", len(products))
	writeJSON(w, http.StatusOK, ListProductsResponse{Data: products, Total: len(products)})
}

// ── intentional error endpoints ───────────────────────────────────────────────

func (s *server) handleErrorPanic(w http.ResponseWriter, r *http.Request) {
	slog.WarnContext(r.Context(), "triggering intentional panic for observability demo")
	panic("intentional panic: simulating an unexpected crash for observability demo")
}

func (s *server) handleErrorNotFound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := errors.New("the requested resource does not exist")
	span := tracelit.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetAttribute("demo.error_type", "not_found")
	slog.WarnContext(ctx, "resource not found demo", "path", "/nonexistent-resource", "error", err)
	writeError(w, r, http.StatusNotFound, "NOT_FOUND", err.Error())
}

func (s *server) handleErrorDB(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)
	span.SetAttribute("demo.error_type", "db_error")

	_, err := s.db.QueryContext(ctx, "SELECT * FROM table_that_does_not_exist_xyz")
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "intentional DB error fired",
			"table", "table_that_does_not_exist_xyz",
			"error", err,
		)
		writeError(w, r, http.StatusInternalServerError, "DB_ERROR", "database error: "+err.Error())
	}
}

func (s *server) handleErrorValidation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := errors.New("multiple validation constraints violated")
	span := tracelit.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetAttributes(map[string]any{
		"demo.error_type":   "validation",
		"validation.fields": "name,price,stock",
	})
	slog.WarnContext(ctx, "validation error demo",
		"violated_fields", []string{"name", "price", "stock"},
		"error", err,
	)
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":    "validation failed",
		"code":     "VALIDATION_ERROR",
		"trace_id": tracelit.TraceID(ctx),
		"fields": []map[string]string{
			{"field": "name", "message": "name is required"},
			{"field": "price", "message": "price must be >= 0"},
			{"field": "stock", "message": "stock must be a non-negative integer"},
		},
	})
}

func (s *server) handleErrorTimeout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := tracelit.SpanFromContext(ctx)
	span.SetAttributes(map[string]any{
		"demo.error_type":  "timeout",
		"timeout.deadline": "50ms",
		"db.query":         "pg_sleep(5)",
	})

	queryCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	_, err := s.db.QueryContext(queryCtx, `SELECT pg_sleep(5)`)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unexpected: query succeeded"})
		return
	}

	span.RecordError(err)
	slog.ErrorContext(ctx, "intentional timeout fired",
		"timeout_ms", 50,
		"query", "pg_sleep(5)",
		"error", err,
	)
	writeError(w, r, http.StatusGatewayTimeout, "TIMEOUT", "query exceeded 50ms deadline: "+err.Error())
}
