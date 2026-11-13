package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	macOSGate        = "github.repository == 'mateusz-klatt/iris' && github.ref == 'refs/heads/main'"
	macOSSigningStep = "Build, sign and notarize the snapshot"
	acceptanceStep   = "Require Apple acceptance for both binaries"
	macOSVerifyStep  = "Verify both signatures and run the arm64 binary"
	snapshotRelease  = `goreleaser release --snapshot --clean --skip=docker,sbom,sign,publish 2>&1 | tee "$RUNNER_TEMP/macos-signing-proof.log"`
)

// macOSSecrets are the Apple secrets the signing step, and only it, receives.
var macOSSecrets = []string{"MACOS_SIGN_P12", "MACOS_SIGN_PASSWORD", "MACOS_NOTARY_ISSUER_ID", "MACOS_NOTARY_KEY_ID", "MACOS_NOTARY_KEY"}

// guardMacOSProof checks macos-signing-proof.yml, then every mutation.
func guardMacOSProof(root string) ([]string, error) {
	proof, err := load(filepath.Join(root, ".github", "workflows", "macos-signing-proof.yml"))
	if err != nil {
		return nil, err
	}
	mutations := macOSMutations(proof)
	if err := verify("macos-signing-proof.yml", proof, checkMacOSProof, mutations); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("macos-signing-proof.yml: real workflow passed; all %d mutations rejected", len(mutations))}, nil
}

func macOSAction(action string) bool { return slices.Contains(macOSPinned, action) }

// checkMacOSProof is the macOS proof's guard: it runs only by hand on main, its Apple secrets
// reach only the signing step, both binaries must be accepted by Apple, and a Mac verifies both
// signatures and runs the arm64 binary.
func checkMacOSProof(proof any) error {
	g := &guard{}
	g.need(equal(at(proof, "on"), map[string]any{"workflow_dispatch": nil}), "the macOS proof must run only by hand")
	g.need(equal(at(proof, "permissions"), map[string]any{"contents": "read"}), "the default permission must be contents: read")
	g.need(equal(at(proof, "concurrency"), map[string]any{"group": "macos-signing-proof", "cancel-in-progress": false}), "proofs must queue, never cancel")
	jobs := at(proof, "jobs")
	g.need(equal(keys(jobs), []string{"sign", "verify", "web"}), "unexpected jobs %v", keys(jobs))
	g.need(at(jobs, "sign", "needs") == "web", "sign must need web")
	g.need(at(jobs, "verify", "needs") == "sign", "verify must need sign")
	g.need(!anyText(proof, func(s string) bool { return strings.Contains(strings.ToLower(s), "certum_") }), "no Certum name may appear in the macOS proof")
	g.need(!anyKey(proof, func(k string) bool { return k == "environment" || k == "continue-on-error" }), "no job or step may name an environment or continue on error")
	g.need(!anyText(proof, func(s string) bool { return strings.Contains(s, "always()") }), "nothing may run with always()")
	for _, name := range keys(jobs) {
		job := at(jobs, name)
		runner, perms := "ubuntu-latest", any(map[string]any{"contents": "read"})
		if name == "verify" {
			runner, perms = "macos-latest", map[string]any{}
		}
		g.need(at(job, "if") == macOSGate, "job %s must run only from main of this repository", name)
		g.need(at(job, "runs-on") == runner, "job %s must run on %s", name, runner)
		g.need(equal(at(job, "permissions"), perms), "job %s has the wrong permissions", name)
		for _, step := range items(at(job, "steps")) {
			g.checkPinned(step, macOSAction)
			if strings.HasPrefix(text(at(step, "uses")), "actions/checkout@") {
				g.need(at(step, "with", "ref") == expression("github.sha"), "a checkout must take the commit that started the run")
			}
		}
		if name == "verify" {
			continue
		}
		g.need(equal(at(job, "outputs"), map[string]any{"artifact-id": expression("steps.upload.outputs.artifact-id")}), "job %s has the wrong outputs", name)
		upload := g.one(stepsUsing(job, "actions/upload-artifact"), "upload in "+name)
		g.need(equal(upload, at(job, "steps", -1)), "job %s's upload must be its last step", name)
		g.need(at(upload, "id") == "upload", "job %s's upload must have the id upload", name)
		g.need(strings.HasSuffix(text(at(upload, "with", "name")), runArtifact), "job %s's artifact must be named for its run", name)
		g.need(at(upload, "with", "retention-days") == 1, "job %s's artifact must be kept one day", name)
		g.need(at(upload, "with", "if-no-files-found") == "error", "job %s's upload must fail without files", name)
	}
	g.need(equal(at(jobs, "web", "steps", 1, "with"), map[string]any{"node-version": 26, "cache": "npm", "cache-dependency-path": "web/package-lock.json"}), "web must set up Node 26 with the npm cache")
	g.need(at(jobs, "web", "steps", 2, "run") == "make web", "web must build the console with make web")
	g.need(at(jobs, "web", "steps", -1, "with", "path") == "internal/webui/dist", "web must hand over only the console")
	sign := at(jobs, "sign")
	g.need(at(sign, "steps", 0, "with", "fetch-depth") == 0, "sign must check out the full history")
	g.need(equal(at(sign, "steps", 2, "with"), map[string]any{"go-version-file": "go.mod"}), "sign must set up Go from go.mod")
	g.need(equal(at(sign, "steps", 3, "with"), map[string]any{"version": "v2.20.0", "install-only": true}), "goreleaser must be v2.20.0, installed only")
	signing := g.one(stepsNamed(sign, macOSSigningStep), "macOS signing step")
	wantEnv := map[string]any{"WEB_DIST": expression("runner.temp") + "/webui-dist"}
	for _, name := range macOSSecrets {
		wantEnv[name] = expression("secrets." + name)
	}
	g.need(equal(at(signing, "env"), wantEnv), "the signing step's env must hold only its secrets and the console")
	run := text(at(signing, "run"))
	for _, policy := range []string{"set -euo pipefail", "for credential in " + strings.Join(macOSSecrets, " ") + "; do", `if [ -z "${!credential}" ]; then`, "exit 1"} {
		g.need(strings.Contains(run, policy), "the signing step must keep %q", policy)
	}
	runLines := lines(run)
	g.need(len(runLines) > 0 && runLines[len(runLines)-1] == snapshotRelease, "the signing step must end with the snapshot release")
	outside := clone(proof)
	for _, step := range stepsNamed(at(outside, "jobs", "sign"), macOSSigningStep) {
		delete(fields(step), "env")
	}
	g.need(!mentionsSecrets(outside), "secrets outside the signing step's env")
	acceptance := text(at(g.one(stepsNamed(sign, acceptanceStep), "acceptance step"), "run"))
	for _, policy := range []string{
		`("iris_darwin_amd64_v1", "iris_darwin_arm64_v8.0")`,
		`binary = f"dist/{target}/iris"`,
		`if not re.search(r"\bnotarized\s+binary=" + re.escape(binary) + r"(?:\s|$)", log):`,
		`raise SystemExit`,
		`GITHUB_STEP_SUMMARY`,
	} {
		g.need(strings.Contains(acceptance, policy), "the acceptance step must keep %q", policy)
	}
	g.need(equal(lines(at(sign, "steps", -1, "with", "path")), []string{"dist/iris_darwin_amd64_v1/iris", "dist/iris_darwin_arm64_v8.0/iris"}), "sign must hand over only the two binaries")
	for _, hop := range []struct{ job, source, path string }{{"sign", "web", "webui-dist"}, {"verify", "sign", "macos-proof"}} {
		var downloads []any
		for _, step := range stepsUsing(at(jobs, hop.job), "actions/download-artifact") {
			downloads = append(downloads, at(step, "with"))
		}
		g.need(equal(downloads, []any{map[string]any{"artifact-ids": expression("needs." + hop.source + ".outputs.artifact-id"), "path": expression("runner.temp") + "/" + hop.path}}), "%s must download exactly %s's artifact by id", hop.job, hop.source)
	}
	verify := at(jobs, "verify")
	g.need(len(items(at(verify, "steps"))) == 2, "verify must have exactly two steps")
	native := text(at(g.one(stepsNamed(verify, macOSVerifyStep), "macOS verification step"), "run"))
	for _, policy := range macOSVerifyPolicies {
		g.need(strings.Contains(native, policy), "the Mac verification must keep %q", policy)
	}
	return g.err
}

// macOSVerifyPolicies are the lines the Mac verification must keep.
var macOSVerifyPolicies = []string{
	`set -euo pipefail`,
	`test "$(uname -m)" = arm64`,
	`test "$(find . -type f | LC_ALL=C sort)" = "$(printf '%s\n' ./iris_darwin_amd64_v1/iris ./iris_darwin_arm64_v8.0/iris)"`,
	`for target in iris_darwin_amd64_v1 iris_darwin_arm64_v8.0; do`,
	`binary="./$target/iris"`,
	`chmod +x "$binary"`,
	`codesign --verify --strict --verbose=2 "$binary" 2>&1 | tee -a "$GITHUB_STEP_SUMMARY"`,
	`codesign -dv --verbose=4 "$binary" 2>&1 | tee "$RUNNER_TEMP/macos-signature.txt" | tee -a "$GITHUB_STEP_SUMMARY"`,
	`grep -Fx 'Authority=Developer ID Application: Mateusz Klatt (26MP7QQP95)'`,
	`grep -Fx 'TeamIdentifier=26MP7QQP95'`,
	`grep -E '^CodeDirectory .*flags=.*\(.*runtime.*\)'`,
	`grep -E '^Timestamp=.+$'`,
	`spctl -a -vvv -t install "$binary" 2>&1 | tee "$RUNNER_TEMP/macos-assessment.txt" | tee -a "$GITHUB_STEP_SUMMARY"`,
	`grep -Fx "$binary: accepted"`,
	`grep -Fx 'source=Notarized Developer ID'`,
	`./iris_darwin_arm64_v8.0/iris version 2>&1 | tee -a "$GITHUB_STEP_SUMMARY"`,
}

// macOSMutations are the weakenings of macos-signing-proof.yml the guard must reject.
func macOSMutations(proof any) []mutation {
	m := []mutation{
		{[]any{"on"}, map[string]any{"push": nil}},
		{[]any{"permissions"}, map[string]any{"contents": "write"}},
		{[]any{"concurrency", "cancel-in-progress"}, true},
		{[]any{"jobs", "sign", "needs"}, []any{}},
		{[]any{"jobs", "verify", "needs"}, "web"},
		{[]any{"jobs", "verify", "runs-on"}, "ubuntu-latest"},
		{[]any{"jobs", "sign", "continue-on-error"}, true},
		{[]any{"jobs", "sign", "environment"}, "signing"},
		{[]any{"jobs", "web", "steps", -1, "with", "path"}, "dist/*"},
		{[]any{"jobs", "sign", "steps", -1, "with", "path"}, "dist/*"},
		{[]any{"jobs", "sign", "steps", 3, "with", "version"}, "latest"},
		{[]any{"jobs", "sign", "steps", 3, "with", "install-only"}, false},
	}
	// The old guard walked the jobs in file order: web, sign, verify.
	for _, name := range []string{"web", "sign", "verify"} {
		m = append(m,
			mutation{[]any{"jobs", name, "if"}, "always()"},
			mutation{[]any{"jobs", name, "permissions"}, map[string]any{"contents": "read", "id-token": "write"}})
		for i, step := range items(at(proof, "jobs", name, "steps")) {
			path := func(rest ...any) []any { return append([]any{"jobs", name, "steps", i}, rest...) }
			uses := text(at(step, "uses"))
			if _, ok := fields(step)["uses"]; ok {
				action, _, _ := strings.Cut(uses, "@")
				m = append(m, mutation{path("uses"), action + "@main"})
			}
			switch {
			case strings.HasPrefix(uses, "actions/checkout@"):
				m = append(m, mutation{path("with", "persist-credentials"), true})
			case strings.HasPrefix(uses, "actions/download-artifact@"):
				m = append(m, mutation{path("with", "artifact-ids"), "1,2"})
			case strings.HasPrefix(uses, "actions/upload-artifact@"):
				m = append(m, mutation{path("with", "retention-days"), 90}, mutation{path("with", "name"), "proof"})
			}
		}
	}
	for _, leak := range []struct {
		path      []any
		reference string
	}{
		{[]any{"env"}, "secrets.MACOS_NOTARY_KEY"},
		{[]any{"jobs", "web", "env"}, "secrets.macos_sign_p12"},
		{[]any{"jobs", "sign", "env"}, "toJSON(secrets)"},
		{[]any{"jobs", "verify", "env"}, "secrets[format('MACOS_{0}', 'NOTARY_KEY')]"},
		{[]any{"jobs", "sign", "steps", 0, "env"}, "secrets.MACOS_SIGN_PASSWORD"},
		{[]any{"jobs", "verify", "steps", 0, "env"}, "toJSON(secrets)"},
	} {
		m = append(m, mutation{leak.path, map[string]any{"LEAK": expression(leak.reference)}})
	}
	signing := []any{"jobs", "sign", "steps", 4}
	for _, leak := range []string{"secrets.CERTUM_OTP", "secrets.GITHUB_TOKEN", "toJSON(secrets)"} {
		m = append(m, mutation{append(slices.Clone(signing), "env", "LEAK"), expression(leak)})
	}
	run := text(at(proof, append(slices.Clone(signing), "run")...))
	for _, r := range [][2]string{
		{"--snapshot ", ""},
		{"docker,sbom,sign,publish", "docker,sbom,sign,publish,notarize"},
		{"set -euo pipefail", "set -eu"},
		{`if [ -z "${!credential}" ]; then`, "if false; then"},
	} {
		m = append(m, mutation{append(slices.Clone(signing), "run"), strings.ReplaceAll(run, r[0], r[1])})
	}
	acceptance := text(at(proof, "jobs", "sign", "steps", 5, "run"))
	for _, original := range []string{"iris_darwin_arm64_v8.0", "notarized", "raise SystemExit"} {
		m = append(m, mutation{[]any{"jobs", "sign", "steps", 5, "run"}, strings.ReplaceAll(acceptance, original, "omitted")})
	}
	native := text(at(proof, "jobs", "verify", "steps", -1, "run"))
	for _, original := range []string{"set -euo pipefail", `test "$(uname -m)" = arm64`, "for target in iris_darwin_amd64_v1 iris_darwin_arm64_v8.0; do", `chmod +x "$binary"`, "--strict", "codesign -dv --verbose=4", "Authority=Developer ID Application: Mateusz Klatt (26MP7QQP95)", "TeamIdentifier=26MP7QQP95", "runtime", "Timestamp", "spctl -a -vvv -t install", "$binary: accepted", "source=Notarized Developer ID", "./iris_darwin_arm64_v8.0/iris version", "GITHUB_STEP_SUMMARY"} {
		m = append(m, mutation{[]any{"jobs", "verify", "steps", -1, "run"}, strings.ReplaceAll(native, original, "omitted")})
	}
	return m
}
