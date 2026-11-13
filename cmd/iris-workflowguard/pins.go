package main

// pins are the reviewed commits the release and proof workflows may use, one per action. A
// workflow step that uses another commit, or an action not listed, fails its check. `make refresh`
// moves these entries together with the workflows' uses: lines (pinTables in .github/refresh), so
// each line keeps the form "owner/repo[/path]": "<commit>". Dependabot does not see this file; a
// Dependabot bump of a release action fails this check until the entry here moves too.
var pins = map[string]string{
	"actions/checkout":                  "3d3c42e5aac5ba805825da76410c181273ba90b1",
	"actions/setup-node":                "949feb2413d6458794dcd2491c4babbbce0c15c1",
	"actions/setup-go":                  "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
	"actions/upload-artifact":           "cf430e030ddbb5b0abf93d22962f4752f3646cd9",
	"actions/download-artifact":         "9000827ccba6bdab643e8b6fd33ac0654aef8333",
	"sigstore/cosign-installer":         "756fe2d284c7292c4e9b62c8a28f1e5629c59532",
	"anchore/sbom-action/download-syft": "66cbf4bc1f1c0d2edc94016e65bc221b6bb0ad6c",
	"goreleaser/goreleaser-action":      "a4f02ab5355a589de60d917a659ce617ed19b3de",
	"actions/attest-build-provenance":   "4d101475d8b20a2381f78447822ac1eab6504dd8",
	"docker/setup-buildx-action":        "f87e5991a6d7451dcb8d9637bfbc97413f497069",
	"docker/login-action":               "8aa5831fb8d48d8f76594c7ddde61ae58019f550",
	"docker/build-push-action":          "c3c9e263c25d99ce0380d002d59b67737d91b0dc",
}

// macOSPinned are the actions the macOS proof may use; its old guard listed only these.
var macOSPinned = []string{
	"actions/checkout", "actions/setup-node", "actions/setup-go",
	"actions/upload-artifact", "actions/download-artifact", "goreleaser/goreleaser-action",
}
