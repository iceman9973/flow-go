module github.com/kodelyx/flow-go/flow-go

go 1.26.4

require (
	github.com/bogdanfinn/fhttp v0.6.9
	github.com/bogdanfinn/tls-client v1.16.0
	github.com/gofiber/fiber/v3 v3.5.0
	github.com/google/uuid v1.6.0
	github.com/joho/godotenv v1.5.1
	github.com/quic-go/quic-go v0.62.0
	modernc.org/sqlite v1.59.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

require (
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/bdandy/go-errors v1.2.2 // indirect
	github.com/bdandy/go-socks4 v1.2.3 // indirect
	github.com/bogdanfinn/quic-go-utls v1.0.10-utls // indirect
	github.com/bogdanfinn/utls v1.7.8-barnius // indirect
	github.com/bogdanfinn/websocket v1.5.6-barnius // indirect
	github.com/cloudflare/circl v1.6.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gofiber/schema v1.8.3 // indirect
	github.com/gofiber/utils/v2 v2.4.1 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/kodelyx/Browser-cdp/cdp-control v0.0.0-20260920054928-9aaab4e4a4f7
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tam7t/hpkp v0.0.0-20160821193359-2b70b4024ed5 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.73.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// browser-Cdp used to be reachable only through a replace pointing at a sibling
// checkout, because its declared module path matched no repository. It now declares
// the path Go derives from where the code lives and is fetched like anything else.
// To work against a local checkout instead, add for the duration:
//
//	replace github.com/kodelyx/Browser-cdp/cdp-control => ../browser-Cdp/cdp-control
