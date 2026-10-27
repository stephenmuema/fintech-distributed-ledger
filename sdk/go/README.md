# Zirnis extensions in Go

The Go SDK is [`pkg/extapi`](../../pkg/extapi). It holds the hook types and interfaces, plus `Handler`, which serves an extension over signed HTTP. The same value can be compiled into the core or run as a separate service.

- **In process:** register it in [`cmd/zirnis/extensions.go`](../../cmd/zirnis/extensions.go) with `sys.App.Extensions.MustRegister(ext)`. Example: [chargeback-classifier](../../examples/extensions/go/chargeback-classifier).
- **Remote:** `http.ListenAndServe(addr, extapi.Handler(ext, secret))`, then register the URL under **Extensions** (maker-checker). Example: [remote-limit-check](../../examples/extensions/go/remote-limit-check).

Implement any of these interfaces:

- `Classifier`
- `Matcher`
- `ClearingValidator`
- `StatementMatcher`
- `CobRunner`

`extapi.Implements(ext)` reports which hooks a value serves. See [ADR 0011](../../docs/adr/0011-extension-framework.md) for the rules: extensions never post, and they are never called inside a write transaction.
