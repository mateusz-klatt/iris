package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	proofSigningStep = "Sign each binary once"
	proofHelperStep  = "Build the release helper"
	helperBuild      = `go build -trimpath -o "$RUNNER_TEMP/iris-release-tools" ./cmd/iris-release-tools`
)

// guardSigningProof checks signing-proof.yml, then every mutation.
func guardSigningProof(root string) ([]string, error) {
	proof, err := load(filepath.Join(root, ".github", "workflows", "signing-proof.yml"))
	if err != nil {
		return nil, err
	}
	mutations, err := signingMutations(proof)
	if err != nil {
		return nil, fmt.Errorf("signing-proof.yml: %w", err)
	}
	if err := verify("signing-proof.yml", proof, checkSigningProof, mutations); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("signing-proof.yml: real workflow passed; all %d mutations rejected", len(mutations))}, nil
}

// checkSigningProof is the signing proof's guard: it runs only by hand, its Certum secrets reach
// only the one signing step, and that step signs each binary in its own helper process.
func checkSigningProof(proof any) error {
	g := &guard{}
	g.need(equal(keys(at(proof, "on")), []string{"workflow_dispatch"}), "the signing proof must run only by hand")
	outside := clone(proof)
	sign := at(outside, "jobs", "sign")
	delete(fields(at(outside, "jobs")), "sign")
	g.need(!anyText(outside, func(s string) bool { return strings.Contains(s, "CERTUM_") }), "Certum credentials outside sign")
	g.need(!mentionsSecrets(outside), "secrets outside sign")
	signing := g.one(stepsNamed(sign, proofSigningStep), "signing step")
	g.need(equal(at(signing, "env"), map[string]any{
		"CERTUM_EMAIL":        expression("secrets.CERTUM_EMAIL"),
		"CERTUM_OTP":          expression("secrets.CERTUM_OTP"),
		"IRIS_WINDOWS_SIGNER": expression("runner.temp") + "/signer/ssign",
		"XDG_RUNTIME_DIR":     expression("runner.temp") + "/certum-signing",
	}), "expected only the signing step's credentials and paths")
	g.need(equal(lines(at(signing, "run")), []string{
		`"$RUNNER_TEMP/iris-release-tools" sign-windows --arch=amd64 --path="$RUNNER_TEMP/windows/amd64/iris.exe"`,
		`"$RUNNER_TEMP/iris-release-tools" sign-windows --arch=arm64 --path="$RUNNER_TEMP/windows/arm64/iris.exe"`,
	}), "expected two separate helper processes")
	native := false
	for _, step := range items(at(sign, "steps")) {
		native = native || (at(step, "run") == helperBuild && equal(at(step, "env"), map[string]any{"CGO_ENABLED": "0"}))
	}
	g.need(native, "expected a native helper build")
	delete(fields(signing), "env")
	g.need(!mentionsSecrets(sign), "secrets outside the signing step's env")
	return g.err
}

// signingMutations are the weakenings of signing-proof.yml the guard must reject.
func signingMutations(proof any) ([]mutation, error) {
	var m []mutation
	for _, leak := range []struct {
		path      []any
		reference string
	}{
		{[]any{"jobs", "verify", "env"}, "secrets.certum_otp"},
		{[]any{"jobs", "signer", "env"}, "secrets[format('certum_{0}', 'otp')]"},
		{[]any{"jobs", "verify", "env"}, "toJSON(secrets)"},
		{[]any{"env"}, "secrets.certum_otp"},
		{[]any{"jobs", "sign", "env"}, "secrets.CERTUM_OTP"},
		{[]any{"jobs", "sign", "steps", 0, "env"}, "secrets.CERTUM_OTP"},
	} {
		m = append(m, mutation{leak.path, map[string]any{"LEAK": expression(leak.reference)}})
	}
	signing, ok := stepIndex(at(proof, "jobs", "sign"), proofSigningStep)
	if !ok {
		return nil, fmt.Errorf("no step named %q", proofSigningStep)
	}
	helper, ok := stepIndex(at(proof, "jobs", "sign"), proofHelperStep)
	if !ok {
		return nil, fmt.Errorf("no step named %q", proofHelperStep)
	}
	return append(m,
		// Direct signing in one process.
		mutation{[]any{"jobs", "sign", "steps", signing, "run"}, `"$RUNNER_TEMP/signer/ssign" "$RUNNER_TEMP/windows/amd64/iris.exe" "$RUNNER_TEMP/windows/arm64/iris.exe"`},
		// A missing helper build.
		mutation{[]any{"jobs", "sign", "steps", helper, "run"}, "echo helper omitted"},
		// Extra signing secrets.
		mutation{[]any{"jobs", "sign", "steps", signing, "env", "LEAK"}, expression("toJSON(secrets)")},
	), nil
}

// stepIndex is the position of the first step of a job with the given name.
func stepIndex(job any, name string) (int, bool) {
	for i, step := range items(at(job, "steps")) {
		if text(at(step, "name")) == name {
			return i, true
		}
	}
	return 0, false
}
