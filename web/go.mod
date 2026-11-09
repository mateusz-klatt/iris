// This Node-only workspace is outside the application's Go module so ./...
// never discovers Go code shipped by npm dependencies. Keep first-party Go
// source outside web/; the buildinfo layout test checks this boundary.
module github.com/mateusz-klatt/iris/web
