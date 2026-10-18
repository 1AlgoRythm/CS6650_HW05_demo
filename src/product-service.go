package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Product matches the Product schema in api.yaml.
type Product struct {
	ProductID    int32  `json:"product_id"`
	SKU          string `json:"sku"`
	Manufacturer string `json:"manufacturer"`
	CategoryID   int32  `json:"category_id"`
	Weight       int32  `json:"weight"`
	SomeOtherID  int32  `json:"some_other_id"`
}

// productInput reads the POST body. The fields are pointers so that a
// missing field (nil) differs from a zero value. Weight may legally be 0.
type productInput struct {
	ProductID    *int32  `json:"product_id"`
	SKU          *string `json:"sku"`
	Manufacturer *string `json:"manufacturer"`
	CategoryID   *int32  `json:"category_id"`
	Weight       *int32  `json:"weight"`
	SomeOtherID  *int32  `json:"some_other_id"`
}

// ErrorResponse matches the Error schema in api.yaml.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// store keeps the products in memory. The lock lets many requests read at
// once, and lets only one request write at a time.
type store struct {
	mu       sync.RWMutex
	products map[int32]Product
}

func newStore() *store {
	return &store{products: map[int32]Product{
		1: {ProductID: 1, SKU: "ABC-123-XYZ", Manufacturer: "Acme Corporation", CategoryID: 456, Weight: 1250, SomeOtherID: 789},
		2: {ProductID: 2, SKU: "DEF-456-UVW", Manufacturer: "Globex Corporation", CategoryID: 457, Weight: 300, SomeOtherID: 790},
		3: {ProductID: 3, SKU: "GHI-789-RST", Manufacturer: "Initech", CategoryID: 458, Weight: 0, SomeOtherID: 791},
	}}
}

func writeError(c *gin.Context, status int, code, message, details string) {
	c.JSON(status, ErrorResponse{Error: code, Message: message, Details: details})
}

// parseID turns the {productId} path part into an int32 of 1 or more.
func parseID(raw string) (int32, bool) {
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 1 {
		return 0, false
	}
	return int32(n), true
}

// validate checks the POST body against the limits in api.yaml.
// It returns the product, and a problem text that is empty when the body is valid.
func validate(in productInput) (Product, string) {
	switch {
	case in.ProductID == nil || in.SKU == nil || in.Manufacturer == nil ||
		in.CategoryID == nil || in.Weight == nil || in.SomeOtherID == nil:
		return Product{}, "all six fields are required: product_id, sku, manufacturer, category_id, weight, some_other_id"
	case *in.ProductID < 1 || *in.CategoryID < 1 || *in.SomeOtherID < 1:
		return Product{}, "product_id, category_id and some_other_id must be 1 or more"
	case len(*in.SKU) < 1 || len(*in.SKU) > 100:
		return Product{}, "sku must be 1 to 100 characters"
	case len(*in.Manufacturer) < 1 || len(*in.Manufacturer) > 200:
		return Product{}, "manufacturer must be 1 to 200 characters"
	case *in.Weight < 0:
		return Product{}, "weight must be 0 or more"
	}
	return Product{
		ProductID: *in.ProductID, SKU: *in.SKU, Manufacturer: *in.Manufacturer,
		CategoryID: *in.CategoryID, Weight: *in.Weight, SomeOtherID: *in.SomeOtherID,
	}, ""
}

// simulateError lets a tester trigger the 500 response on purpose.
// Send the header  X-Simulate-Error: true.  api.yaml does not define this header.
func simulateError(c *gin.Context) bool {
	if c.GetHeader("X-Simulate-Error") != "true" {
		return false
	}
	writeError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Simulated server error", "the X-Simulate-Error header was set")
	return true
}

func (s *store) getProduct(c *gin.Context) {
	if simulateError(c) {
		return
	}
	id, ok := parseID(c.Param("productId"))
	if !ok {
		// api.yaml lists no 400 for GET, so a bad id is reported as not found.
		writeError(c, http.StatusNotFound, "NOT_FOUND", "Product not found", "productId must be an integer of 1 or more")
		return
	}
	s.mu.RLock()
	p, found := s.products[id]
	s.mu.RUnlock()
	if !found {
		writeError(c, http.StatusNotFound, "NOT_FOUND", "Product not found", "")
		return
	}
	c.JSON(http.StatusOK, p)
}

func (s *store) addProductDetails(c *gin.Context) {
	if simulateError(c) {
		return
	}
	id, ok := parseID(c.Param("productId"))
	if !ok {
		writeError(c, http.StatusBadRequest, "INVALID_INPUT", "The provided input data is invalid", "productId must be an integer of 1 or more")
		return
	}
	var in productInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_INPUT", "The provided input data is invalid", "the body is not valid JSON for a Product")
		return
	}
	p, problem := validate(in)
	if problem != "" {
		writeError(c, http.StatusBadRequest, "INVALID_INPUT", "The provided input data is invalid", problem)
		return
	}
	// api.yaml does not say what to do when the body product_id differs from the path.
	// This server treats that as invalid input.
	if p.ProductID != id {
		writeError(c, http.StatusBadRequest, "INVALID_INPUT", "The provided input data is invalid", "product_id in the body must match productId in the path")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.products[id]; !found {
		writeError(c, http.StatusNotFound, "NOT_FOUND", "Product not found", "")
		return
	}
	s.products[id] = p
	c.Status(http.StatusNoContent)
}

// The three RED signals for every endpoint: Rate, Errors and Duration.
var (
	// Rate: one count per request. Requests per second is the rate of this counter.
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requests handled, by method, endpoint and status code.",
	}, []string{"method", "endpoint", "status"})

	// Errors: one count per response that is not 2xx.
	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_request_errors_total",
		Help: "Responses that were not 2xx, by method and endpoint.",
	}, []string{"method", "endpoint"})

	// Duration: how long each request took. The buckets start below 1 ms because
	// the server answers in microseconds. A histogram lets Prometheus work out p50 and p95.
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Time to handle a request, by method and endpoint.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"method", "endpoint"})
)

// observe records the RED signals and writes one JSON log line for every request.
// endpoint is the route template, such as /products/:productId, so ids do not make
// a new label value for every product.
func observe(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if c.Request.URL.Path == "/metrics" {
			return // do not count Prometheus scraping itself
		}
		endpoint := c.FullPath()
		if endpoint == "" {
			endpoint = "unmatched"
		}
		method := c.Request.Method
		status := c.Writer.Status()
		elapsed := time.Since(start)

		requestsTotal.WithLabelValues(method, endpoint, strconv.Itoa(status)).Inc()
		if status < 200 || status > 299 {
			errorsTotal.WithLabelValues(method, endpoint).Inc()
		}
		requestDuration.WithLabelValues(method, endpoint).Observe(elapsed.Seconds())

		logger.Info("request",
			"method", method,
			"endpoint", endpoint,
			"path", c.Request.URL.Path,
			"status", status,
			"duration_ms", float64(elapsed.Microseconds())/1000,
		)
	}
}

func newRouter(s *store, logger *slog.Logger) *gin.Engine {
	r := gin.New()
	// observe is first, so it also sees the 500 that the recovery handler writes after a panic.
	r.Use(observe(logger))
	// A panic in a handler becomes the 500 response from api.yaml, with an Error body.
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", "")
	}))
	r.GET("/products/:productId", s.getProduct)
	r.POST("/products/:productId/details", s.addProductDetails)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// Port 8080 is the container_port default that Part III uses.
	if err := newRouter(newStore(), logger).Run(":8080"); err != nil {
		panic(err)
	}
}
