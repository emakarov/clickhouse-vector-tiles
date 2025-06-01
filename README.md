Simple example on how to use clickhouse as a Mapbox Vector Tile server
through intermediate golang webserver integrated with MapboxGL.

This example contains golang-based server and environment variables
to connect to ClickHouse playground (Cell towers database).

How to test this example
1. Install go
2. Install dependencies, build the tile server and run it.
3. This example contains maps rendering in maplibre and mapboxgl
In order to use mapbox, register your access token on mapbox.com
and put it to .env.clickhouse.play ENV var file.

```
go get github.com/ClickHouse/clickhouse-go/v2@v2.16.0
go get github.com/go-chi/chi/v5@v5.0.11
go get github.com/paulmach/orb@v0.11.0
go build -o tile-server main.go
source .env.clickhouse.play
./tile-server
```

Logs example of successful tile server launch

```
2025/06/01 16:18:16 Detected ClickHouse Playground, configuring secure connection and user 'explorer'.
2025/06/01 16:18:16 Connecting to ClickHouse...
2025/06/01 16:18:16   Address: play.clickhouse.com:9440
2025/06/01 16:18:16   User: explorer
2025/06/01 16:18:16   Secure: true
2025/06/01 16:18:16   Playground: true
2025/06/01 16:18:16 Using secure TLS connection
2025/06/01 16:18:16 Testing connection...
2025/06/01 16:18:17 Successfully connected to ClickHouse!
2025/06/01 16:18:17 Testing query...
2025/06/01 16:18:17 Test query successful! Found 43276158 cell towers
2025/06/01 16:18:17 Server starting on :8085
2025/06/01 16:18:17 ClickHouse: play.clickhouse.com:9440 (Secure: true, Playground: true)
2025/06/01 16:18:17 Index page: http://localhost:8085
2025/06/01 16:18:17 Health check: http://localhost:8085/health
2025/06/01 16:18:17 Example tile: http://localhost:8085/tiles/2/2/1.mvt
2025/06/01 16:18:17 MapLibre viewer: http://localhost:8085/maplibre
2025/06/01 16:18:17 Mapbox GL viewer: http://localhost:8085/mapboxgl
```

3. Open http://localhost:8085/maplibre or http://localhost:8085/mapboxgl in your browser



https://github.com/user-attachments/assets/ff9bb305-2216-4fd7-86c3-90444d61af29

