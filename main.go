// Package main implements a tile server that fetches data from ClickHouse
// and serves it as Mapbox Vector Tiles (MVT).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/simplify"
)

// Config holds the application configuration settings.
type Config struct {
	ClickHouseAddr   string        // Address of the ClickHouse server (e.g., "localhost:9000").
	ClickHouseUser   string        // Username for ClickHouse authentication.
	ClickHousePass   string        // Password for ClickHouse authentication.
	UseSecure        bool          // Whether to use a secure TLS connection to ClickHouse.
	ServerPort       string        // Port for the HTTP server to listen on (e.g., ":8080").
	CacheTTL         time.Duration // Time-to-live for cached tiles.
	MaxCacheSize     int           // Maximum number of tiles to store in the cache.
	QueryTimeout     time.Duration // Timeout duration for ClickHouse queries.
	MaxPointsPerTile int           // Maximum number of points to include in a single tile.
	IsPlayground     bool          // Flag indicating if the server is connecting to ClickHouse Playground.
	MapboxToken      string        // Mapbox API token for clients using Mapbox GL JS.
}

// TileCache implements a simple in-memory cache for MVT tiles.
// It uses a map to store tiles and a read-write mutex for concurrent access.
type TileCache struct {
	mu      sync.RWMutex           // Mutex to protect concurrent access to the tiles map.
	tiles   map[string]*CachedTile // Map storing cached tiles, keyed by tile coordinates (e.g., "z/x/y").
	maxSize int                    // Maximum number of tiles the cache can hold.
}

// CachedTile represents a tile stored in the cache.
type CachedTile struct {
	Data      []byte        // The MVT tile data, gzipped.
	CreatedAt time.Time     // Timestamp when the tile was added to the cache.
	TTL       time.Duration // Time-to-live for this specific tile.
}

// Server holds the application state, including the database connection,
// configuration, and tile cache.
type Server struct {
	db     driver.Conn // ClickHouse database connection.
	config Config      // Application configuration.
	cache  *TileCache  // In-memory tile cache.
}

// CellTower represents the structure of data for a cell tower
// as queried from ClickHouse. It includes pixel coordinates for rendering
// within a tile, as well as geographic coordinates.
type CellTower struct {
	PixelX float64 `ch:"pixel_x"` // X-coordinate within the tile (0-4095).
	PixelY float64 `ch:"pixel_y"` // Y-coordinate within the tile (0-4095).
	Lat    float64 `ch:"lat"`     // Latitude of the cell tower.
	Lon    float64 `ch:"lon"`     // Longitude of the cell tower.
}

// NewTileCache creates and initializes a new TileCache with the specified maximum size.
// It also starts a background goroutine to periodically clean up expired tiles.
func NewTileCache(maxSize int) *TileCache {
	cache := &TileCache{
		tiles:   make(map[string]*CachedTile),
		maxSize: maxSize,
	}

	// Start a goroutine that periodically removes expired tiles from the cache.
	go cache.cleanup()

	return cache
}

// Get retrieves a tile from the cache by its key.
// It returns the tile data and true if the tile is found and not expired,
// otherwise it returns nil and false.
func (tc *TileCache) Get(key string) ([]byte, bool) {
	tc.mu.RLock()         // Acquire read lock.
	defer tc.mu.RUnlock() // Release read lock when function returns.

	tile, exists := tc.tiles[key]
	if !exists {
		return nil, false // Tile not found.
	}

	// Check if the tile has expired.
	if time.Since(tile.CreatedAt) > tile.TTL {
		// Tile is expired, conceptually remove it (actual removal happens in cleanup or Set).
		return nil, false
	}

	return tile.Data, true // Tile found and is valid.
}

// Set adds a tile to the cache with the given key, data, and TTL.
// If the cache is full, it removes the oldest entry (Least Recently Used - LRU).
func (tc *TileCache) Set(key string, data []byte, ttl time.Duration) {
	tc.mu.Lock()         // Acquire write lock.
	defer tc.mu.Unlock() // Release write lock when function returns.

	// Simple LRU eviction strategy: if cache is full, remove the oldest entry.
	if len(tc.tiles) >= tc.maxSize && tc.maxSize > 0 { // Ensure maxSize > 0 to prevent issues if maxSize is 0
		var oldestKey string
		var oldestTime time.Time = time.Now().Add(24 * time.Hour) // Initialize with a future time

		for k, v := range tc.tiles {
			if v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
			}
		}
		// oldestKey might be empty if all items have the exact same CreatedAt, or if cache is empty.
		// However, given the check for len(tc.tiles) >= tc.maxSize, it should find an item if maxSize > 0.
		if oldestKey != "" {
			delete(tc.tiles, oldestKey)
		}
	}

	// Add the new tile to the cache.
	tc.tiles[key] = &CachedTile{
		Data:      data,
		CreatedAt: time.Now(),
		TTL:       ttl,
	}
}

// cleanup is a background goroutine that periodically scans the cache
// and removes expired tiles. It runs every 5 minutes.
func (tc *TileCache) cleanup() {
	ticker := time.NewTicker(5 * time.Minute) // Create a ticker that fires every 5 minutes.
	defer ticker.Stop()                       // Ensure the ticker is stopped when the goroutine exits.

	for range ticker.C { // Loop indefinitely, executing on each tick.
		tc.mu.Lock() // Acquire write lock to modify the cache.
		now := time.Now()
		for key, tile := range tc.tiles {
			if now.Sub(tile.CreatedAt) > tile.TTL {
				delete(tc.tiles, key) // Remove expired tile.
			}
		}
		tc.mu.Unlock() // Release write lock.
	}
}

// NewServer creates a new Server instance, establishing a connection to ClickHouse
// and initializing the tile cache.
func NewServer(config Config) (*Server, error) {
	log.Printf("Connecting to ClickHouse...")
	log.Printf("  Address: %s", config.ClickHouseAddr)
	log.Printf("  User: %s", config.ClickHouseUser)
	log.Printf("  Secure: %v", config.UseSecure)
	log.Printf("  Playground: %v", config.IsPlayground)

	// Configure ClickHouse connection options.
	options := &clickhouse.Options{
		Addr: []string{config.ClickHouseAddr}, // ClickHouse server addresses.
		Auth: clickhouse.Auth{ // Authentication details.
			Database: "default",
			Username: config.ClickHouseUser,
			Password: config.ClickHousePass,
		},
		Compression: &clickhouse.Compression{ // Enable LZ4 compression.
			Method: clickhouse.CompressionLZ4,
		},
		Protocol:        clickhouse.Native, // Use Native ClickHouse protocol.
		MaxOpenConns:    10,                // Max open connections to the database.
		MaxIdleConns:    5,                 // Max idle connections in the pool.
		ConnMaxLifetime: time.Hour,         // Max lifetime of a connection.
		DialTimeout:     10 * time.Second,  // Timeout for establishing a connection.
		Debug:           false,             // Disable debug logging for the driver.
	}

	// Apply specific settings if not connecting to the read-only ClickHouse Playground.
	if !config.IsPlayground {
		options.Settings = clickhouse.Settings{
			"max_execution_time": 60,           // Max query execution time in seconds.
			"max_threads":        8,            // Max threads for query processing.
			"max_memory_usage":   "2000000000", // Max memory usage for a query (2GB).
		}
	}

	// Configure TLS if secure connection is enabled.
	if config.UseSecure {
		log.Printf("Using secure TLS connection")
		options.TLS = &tls.Config{
			InsecureSkipVerify: false, // Set to true only for testing with self-signed certs.
		}
	}

	// Open the connection to ClickHouse.
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to ClickHouse: %w", err)
	}

	log.Printf("Testing connection...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Ping the database to verify the connection.
	if err := conn.Ping(ctx); err != nil {
		// Close the connection if ping fails.
		if cerr := conn.Close(); cerr != nil {
			log.Printf("Error closing connection after ping failure: %v", cerr)
		}
		return nil, fmt.Errorf("failed to ping ClickHouse: %w", err)
	}

	log.Printf("Successfully connected to ClickHouse!")

	// Perform a test query to ensure the 'cell_towers' table is accessible.
	log.Printf("Testing query...")
	var count uint64
	// Query for the total count of cell towers.
	row := conn.QueryRow(ctx, "SELECT count() FROM default.cell_towers")
	if err := row.Scan(&count); err != nil {
		// Log a warning if the test query fails, but don't prevent server startup.
		log.Printf("Warning: test query failed: %v. This might be due to the table not existing or permissions.", err)
	} else {
		log.Printf("Test query successful! Found %d cell towers", count)
	}

	// Return the initialized Server instance.
	return &Server{
		db:     conn,
		config: config,
		cache:  NewTileCache(config.MaxCacheSize),
	}, nil
}

// getTileHandler handles requests for MVT tiles.
// It parses tile coordinates (z, x, y) from the URL, checks the cache,
// queries ClickHouse if not found, generates the MVT, caches it, and sends it.
func (s *Server) getTileHandler(w http.ResponseWriter, r *http.Request) {
	// Parse zoom level (z) from URL parameter.
	z, err := strconv.Atoi(chi.URLParam(r, "z"))
	if err != nil || z < 0 || z > 24 { // Validate zoom level (common range for web maps).
		http.Error(w, "Invalid zoom level", http.StatusBadRequest)
		return
	}

	// Parse x coordinate from URL parameter.
	x, err := strconv.Atoi(chi.URLParam(r, "x"))
	if err != nil {
		http.Error(w, "Invalid x coordinate", http.StatusBadRequest)
		return
	}

	// Parse y coordinate from URL parameter.
	y, err := strconv.Atoi(chi.URLParam(r, "y"))
	if err != nil {
		http.Error(w, "Invalid y coordinate", http.StatusBadRequest)
		return
	}

	// Validate tile coordinates against the zoom level.
	// The maximum tile index at a given zoom level is 2^z - 1.
	maxTile := (1 << z) - 1
	if x < 0 || x > maxTile || y < 0 || y > maxTile {
		http.Error(w, "Tile coordinates out of range for the given zoom level", http.StatusBadRequest)
		return
	}

	// Generate cache key for the requested tile.
	cacheKey := fmt.Sprintf("%d/%d/%d", z, x, y)

	// Attempt to retrieve the tile from the cache.
	if data, found := s.cache.Get(cacheKey); found {
		s.sendTileResponse(w, data) // Send cached tile.
		return
	}

	// If tile not in cache, query ClickHouse.
	// Create a context with a timeout for the database query.
	ctx, cancel := context.WithTimeout(r.Context(), s.config.QueryTimeout)
	defer cancel() // Ensure the context is canceled to release resources.

	// Optimize the number of points fetched based on zoom level.
	// Fetch fewer points at lower zoom levels (more zoomed out) for performance.
	limit := s.config.MaxPointsPerTile
	if z < 10 {
		limit = limit / 4 // Reduce limit for z < 10.
	} else if z < 5 {
		limit = limit / 8 // Further reduce for z < 5.
	}

	// Construct the ClickHouse query to fetch cell tower data for the tile.
	// The query calculates Mercator projection and pixel coordinates within the tile.
	// Parameters are inlined using fmt.Sprintf for compatibility with ClickHouse Playground,
	// which might have limitations with prepared statements or parameterized queries via HTTP interface.
	// WARNING: In a production system with direct user input influencing query structure,
	// this approach (inlining parameters) can be vulnerable to SQL injection if inputs are not strictly validated.
	// Here, z, x, y are integers and validated, reducing risk.
	query := fmt.Sprintf(`
		WITH
			bitShiftLeft(1::UInt64, %d) AS zoom_factor,
			bitShiftLeft(1::UInt64, 32 - %d) AS tile_size_mercator_units, -- Size of one tile in global Mercator units at this zoom
			tile_size_mercator_units * %d AS tile_x_begin,    -- Min X Mercator coordinate for this tile
			tile_size_mercator_units * (%d + 1) AS tile_x_end, -- Max X Mercator coordinate for this tile
			tile_size_mercator_units * %d AS tile_y_begin,    -- Min Y Mercator coordinate for this tile (inverted for typical map Y)
			tile_size_mercator_units * (%d + 1) AS tile_y_end, -- Max Y Mercator coordinate for this tile
			-- Convert lat/lon to global Mercator coordinates (scaled to 0 - 2^32 range)
			-- X: (lon + 180) / 360 * 2^32
			bitShiftLeft(toUInt32((lon + 180) / 360 * 4294967296), 0) AS mercator_x_global,
			-- Y: (0.5 - log(tan(lat_rad/2 + PI/4)) / (2*PI)) * 2^32
			bitShiftLeft(toUInt32((0.5 - log(tan(radians(lat) / 2 + pi() / 4)) / (2 * pi())) * 4294967296), 0) AS mercator_y_global,
			-- Calculate pixel coordinates within the MVT tile (typically 0-4095)
			-- Normalize global Mercator coordinate to be relative to the tile's origin, then scale to MVT extent.
			round((mercator_x_global - tile_x_begin) * 4096 / tile_size_mercator_units) AS pixel_x,
			round((mercator_y_global - tile_y_begin) * 4096 / tile_size_mercator_units) AS pixel_y
		SELECT
			pixel_x,
			pixel_y,
			lat,  -- Include original lat/lon for GeoJSON feature creation
			lon
		FROM default.cell_towers
		WHERE
			mercator_x_global >= tile_x_begin AND mercator_x_global < tile_x_end AND
			mercator_y_global >= tile_y_begin AND mercator_y_global < tile_y_end AND
			-- Ensure calculated pixel coordinates are within the MVT extent.
			-- This acts as a secondary filter and handles potential floating point inaccuracies.
			pixel_x >= 0 AND pixel_x <= 4096 AND
			pixel_y >= 0 AND pixel_y <= 4096
		LIMIT %d
	`, z, z, x, x, y, y, limit) // Pass validated z, x, y, and calculated limit.

	// Execute the query.
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		log.Printf("Query error for tile %s: %v", cacheKey, err)
		http.Error(w, "Failed to query database", http.StatusInternalServerError)
		return
	}
	defer rows.Close() // Ensure rows are closed.

	// Collect query results into a slice of CellTower structs.
	towers := make([]CellTower, 0, limit) // Pre-allocate slice capacity.
	for rows.Next() {
		var tower CellTower
		if err := rows.ScanStruct(&tower); err != nil {
			log.Printf("Scan error while reading tower data for tile %s: %v", cacheKey, err)
			// Continue processing other rows if possible.
			continue
		}
		towers = append(towers, tower)
	}

	// Check for errors during row iteration.
	if err := rows.Err(); err != nil {
		log.Printf("Rows error after iterating for tile %s: %v", cacheKey, err)
		http.Error(w, "Failed to process results from database", http.StatusInternalServerError)
		return
	}

	log.Printf("Tile %s: fetched %d towers from DB", cacheKey, len(towers))

	// Create MVT tile data.
	var mvtData []byte
	if len(towers) == 0 {
		// If no towers, create an empty MVT tile.
		// Some clients expect a valid (empty) tile rather than a 404 or error.
		mvtData = []byte{}
	} else {
		// Convert CellTower data to GeoJSON features.
		// MVT libraries often work with GeoJSON as an intermediate format.
		fc := geojson.NewFeatureCollection()
		for i, tower := range towers {
			// Create a GeoJSON point feature using the original latitude and longitude.
			// The MVT library will handle projecting this to tile coordinates.
			point := orb.Point{tower.Lon, tower.Lat}
			feature := geojson.NewFeature(point)
			feature.Properties = geojson.Properties{
				"id": i, // Add a simple ID property.
				// Potentially add other properties from 'tower' if needed in the MVT.
			}
			fc.Append(feature)
		}

		// Create MVT layers. A layer is a collection of features.
		// Here, all towers are placed in a single layer named "stops".
		layers := mvt.NewLayers(map[string]*geojson.FeatureCollection{
			"stops": fc,
		})

		// Project GeoJSON coordinates to MVT tile coordinates.
		// This transforms the geographic coordinates (lat/lon) into the tile's local coordinate system (e.g., 0-4095).
		tileForProjection := maptile.New(uint32(x), uint32(y), maptile.Zoom(z))
		layers.ProjectToTile(tileForProjection)

		// Simplify geometries if the zoom level is low (more zoomed out).
		// This reduces tile size and rendering load.
		// DouglasPeucker simplification is a common algorithm.
		if z < 14 { // Apply simplification for zoom levels less than 14.
			// The tolerance value (1.0 here) depends on the coordinate system and desired level of simplification.
			layers.Simplify(simplify.DouglasPeucker(1.0))
		}

		// Clip features to the tile boundaries.
		// This ensures that geometries do not extend beyond the MVT's defined extent.
		layers.Clip(mvt.MapboxGLDefaultExtentBound) // Uses standard Mapbox GL extent.

		// Marshal the layers into gzipped MVT format.
		var marshalErr error
		mvtData, marshalErr = mvt.MarshalGzipped(layers)
		if marshalErr != nil {
			log.Printf("MVT encoding error for tile %s: %v", cacheKey, marshalErr)
			http.Error(w, "Failed to encode tile", http.StatusInternalServerError)
			return
		}
	}

	// Cache the newly generated tile.
	s.cache.Set(cacheKey, mvtData, s.config.CacheTTL)

	// Send the MVT tile data in the HTTP response.
	s.sendTileResponse(w, mvtData)
}

// sendTileResponse writes the MVT data to the HTTP response writer
// with appropriate headers.
func (s *Server) sendTileResponse(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	// Set Content-Encoding if data is actually gzipped.
	// Empty tiles (len(data) == 0) are not gzipped.
	if len(data) > 0 {
		w.Header().Set("Content-Encoding", "gzip")
	}
	// Set Cache-Control header to allow client-side and proxy caching.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.config.CacheTTL.Seconds())))
	// Set Access-Control-Allow-Origin to allow cross-origin requests (e.g., from different domains).
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(data)
	if err != nil {
		log.Printf("Error writing tile response: %v", err)
	}
}

// healthHandler provides a health check endpoint.
// It pings the ClickHouse database and returns the status.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second) // Short timeout for health check.
	defer cancel()

	status := "ok"
	// Ping the database to check its health.
	if err := s.db.Ping(ctx); err != nil {
		status = "unhealthy"
		log.Printf("Health check failed: ClickHouse ping error: %v", err)
		w.WriteHeader(http.StatusServiceUnavailable) // Return 503 if DB is unhealthy.
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// Respond with JSON indicating status and cache size.
	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"status":     status,
		"cache_size": len(s.cache.tiles), // Provide current cache size.
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding health response: %v", err)
	}
}

// indexHandler serves the index.html example page.
func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// maplibreHandler serves the maplibre.html example page.
func (s *Server) maplibreHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "maplibre.html")
}

// mapboxglHandler serves the mapboxgl.html example page.
func (s *Server) mapboxglHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "mapboxgl.html")
}

// styleHandler serves the style.json for map rendering.
func (s *Server) styleHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "style.json")
}

// configHandler serves client-side configuration, like the Mapbox token.
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	clientConfig := map[string]interface{}{
		"mapboxToken": s.config.MapboxToken,
		// Add other client-side relevant configs here if needed
	}
	if err := json.NewEncoder(w).Encode(clientConfig); err != nil {
		log.Printf("Error encoding config response: %v", err)
	}
}

// main is the entry point of the application.
// It initializes the configuration, server, routes, and starts the HTTP server.
func main() {
	// Load configuration from environment variables with default values.
	config := Config{
		ClickHouseAddr:   getEnv("CLICKHOUSE_ADDR", "localhost:9000"),
		ClickHouseUser:   getEnv("CLICKHOUSE_USER", "default"),
		ClickHousePass:   getEnv("CLICKHOUSE_PASS", ""),
		UseSecure:        getEnvBool("CLICKHOUSE_USE_SECURE", false),
		ServerPort:       getEnv("SERVER_PORT", ":8080"),
		CacheTTL:         parseDuration(getEnv("CACHE_TTL", "1h"), time.Hour),
		MaxCacheSize:     parseInt(getEnv("MAX_CACHE_SIZE", "10000"), 10000),
		QueryTimeout:     parseDuration(getEnv("QUERY_TIMEOUT", "30s"), 30*time.Second),
		MaxPointsPerTile: parseInt(getEnv("MAX_POINTS_PER_TILE", "10000"), 10000),
		IsPlayground:     getEnvBool("CLICKHOUSE_PLAYGROUND", false),
		MapboxToken:      getEnv("MAPBOX_TOKEN", ""), // Token for Mapbox GL JS example
	}

	// Special handling for ClickHouse Playground.
	// If the address matches play.clickhouse.com, override certain settings.
	if config.ClickHouseAddr == "play.clickhouse.com:9440" {
		log.Printf("Detected ClickHouse Playground, configuring secure connection and user 'explorer'.")
		config.UseSecure = true
		config.ClickHouseUser = "explorer" // Playground often uses 'explorer' user.
		config.ClickHousePass = ""         // Playground 'explorer' user usually has no password.
		config.IsPlayground = true
	}

	// Create a new server instance.
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	// Defer closing the database connection until main function exits.
	defer func() {
		if err := server.db.Close(); err != nil {
			log.Printf("Error closing ClickHouse connection: %v", err)
		}
	}()

	// Set up the HTTP router using chi.
	r := chi.NewRouter()

	// Middleware stack:
	r.Use(middleware.RealIP)                    // Sets X-Real-IP header.
	r.Use(middleware.Logger)                    // Logs HTTP requests.
	r.Use(middleware.Recoverer)                 // Recovers from panics and returns 500 error.
	r.Use(middleware.Compress(5))               // Compresses responses (gzip). Level 5 is a good default.
	r.Use(middleware.Timeout(60 * time.Second)) // Sets a timeout for requests.

	// Define HTTP routes:
	r.Get("/tiles/{z}/{x}/{y}.mvt", server.getTileHandler) // Tile endpoint.
	r.Get("/health", server.healthHandler)                 // Health check endpoint.
	r.Get("/", server.indexHandler)                        // Serves MapLibre GL JS example HTML.
	r.Get("/maplibre", server.maplibreHandler)             // Serves MapLibre GL JS example HTML.
	r.Get("/mapboxgl", server.mapboxglHandler)             // Serves Mapbox GL JS example HTML.
	r.Get("/style.json", server.styleHandler)              // Serves the map style JSON.
	r.Get("/config", server.configHandler)                 // Serves client-side configuration.

	// Log server startup information.
	log.Printf("Server starting on %s", config.ServerPort)
	log.Printf("ClickHouse: %s (Secure: %v, Playground: %v)",
		config.ClickHouseAddr, config.UseSecure, config.IsPlayground)
	log.Printf("Index page: http://localhost%s", config.ServerPort)
	log.Printf("Health check: http://localhost%s/health", config.ServerPort)
	log.Printf("Example tile: http://localhost%s/tiles/2/2/1.mvt", config.ServerPort) // Example tile URL.
	log.Printf("MapLibre viewer: http://localhost%s/maplibre", config.ServerPort)
	log.Printf("Mapbox GL viewer: http://localhost%s/mapboxgl", config.ServerPort)
	if config.MapboxToken == "" {
		log.Printf("Warning: MAPBOX_TOKEN environment variable is not set. The Mapbox GL JS viewer might not work as expected.")
	}

	// Start the HTTP server.
	if err := http.ListenAndServe(config.ServerPort, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// Helper functions for reading environment variables with defaults.

// getEnv retrieves an environment variable by key or returns a default value.
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool retrieves a boolean environment variable or returns a default.
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		b, err := strconv.ParseBool(value)
		if err == nil {
			return b
		}
		log.Printf("Warning: could not parse boolean env var %s value %s. Using default %v. Error: %v", key, value, defaultValue, err)
	}
	return defaultValue
}

// parseInt retrieves an integer environment variable or returns a default.
func parseInt(s string, defaultValue int) int {
	// This function takes a string 's' directly, not an env key.
	// It's used with getEnv: parseInt(getEnv("KEY", "default_str_val"), default_int_val)
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	// Log if 's' was not the default string representation of defaultValue
	// to indicate that a user-provided value failed to parse.
	if s != strconv.Itoa(defaultValue) { // Avoid logging if it's just parsing the default string
		log.Printf("Warning: could not parse integer from string '%s'. Using default %d.", s, defaultValue)
	}
	return defaultValue
}

// parseDuration retrieves a time.Duration environment variable or returns a default.
func parseDuration(s string, defaultValue time.Duration) time.Duration {
	// Similar to parseInt, this takes a string 's'.
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	if s != defaultValue.String() { // Avoid logging if it's just parsing the default string
		log.Printf("Warning: could not parse duration from string '%s'. Using default %v.", s, defaultValue.String())
	}
	return defaultValue
}
