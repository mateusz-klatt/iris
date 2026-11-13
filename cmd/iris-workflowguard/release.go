package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	repoOnly = "github.repository == 'mateusz-klatt/iris'"
	// The image is published only from this repository, for a pushed v* tag.
	imageGate          = "github.repository == 'mateusz-klatt/iris' && github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')"
	releaseSigningStep = "Build and sign the draft release"
	extractStep        = "Check and extract the released Linux binaries"
	signImageStep      = "Sign the multi-arch image by digest"
	signerThumbprint   = "9C837C68415187A72D88612BDF14FCB0D6623D67"
	// goreleaserHook signs each Windows binary once, never for a snapshot.
	goreleaserHook = `{{ if and (eq .Os "windows") (not .IsSnapshot) }}"{{ .Env.IRIS_RELEASE_HELPER }}" sign-windows --arch={{ .Arch }} --path="{{ .Path }}"{{ end }}`
)

var (
	versionTag   = "ghcr.io/mateusz-klatt/iris:" + expression("github.ref_name")
	stableLatest = expression("!contains(github.ref_name, '-') && 'ghcr.io/mateusz-klatt/iris:latest' || ''")
	// releaseSecrets are the secrets the signing step, and only it, receives.
	releaseSecrets = []string{"GITHUB_TOKEN", "CERTUM_EMAIL", "CERTUM_OTP", "MACOS_SIGN_P12", "MACOS_SIGN_PASSWORD", "MACOS_NOTARY_ISSUER_ID", "MACOS_NOTARY_KEY_ID", "MACOS_NOTARY_KEY"}
	runArtifact    = expression("github.run_id") + "-" + expression("github.run_attempt")
	helperVer      = expression("steps.helper.outputs.version")
)

// guardRelease checks release.yml with .goreleaser.yaml and signing-proof.yml, then every mutation.
func guardRelease(root string) ([]string, error) {
	release, err := load(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		return nil, err
	}
	config, err := load(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		return nil, err
	}
	proof, err := load(filepath.Join(root, ".github", "workflows", "signing-proof.yml"))
	if err != nil {
		return nil, err
	}
	// The workflow and its goreleaser config are checked, and mutated, as one document, so a
	// mutation of either is rejected by the same check.
	both := map[string]any{"release": release, "config": config}
	mutations := prefixed("release", releaseMutations(release))
	mutations = append(mutations, prefixed("config", configMutations())...)
	check := func(doc any) error { return checkRelease(at(doc, "release"), at(doc, "config"), proof) }
	if err := verify("release.yml", both, check, mutations); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("release.yml: real workflow and config passed; all %d mutations rejected", len(mutations))}, nil
}

// prefixed puts each mutation's path under key.
func prefixed(key string, mutations []mutation) []mutation {
	out := make([]mutation, len(mutations))
	for i, m := range mutations {
		out[i] = mutation{append([]any{key}, m.path...), m.value}
	}
	return out
}

// checkPinned checks that a step uses only a pinned action from allowed, and that a checkout
// keeps no credentials.
func (g *guard) checkPinned(step any, allowed func(string) bool) {
	if uses, ok := fields(step)["uses"]; ok {
		action, revision, found := strings.Cut(text(uses), "@")
		g.need(found && allowed(action) && pins[action] != "" && revision == pins[action], "%v is not pinned to its reviewed commit", uses)
	}
	if strings.HasPrefix(text(at(step, "uses")), "actions/checkout@") {
		g.need(at(step, "with", "persist-credentials") == false, "a checkout keeps its credentials")
	}
}

func anyAction(string) bool { return true }

// checkRelease is the release guard: release.yml must run only for this repository's v* tags,
// sign in one step that alone holds the signing secrets, and publish the image only from its own
// gated job. config and proof are .goreleaser.yaml and signing-proof.yml.
func checkRelease(release, config, proof any) error {
	g := &guard{}
	g.need(equal(at(release, "on"), map[string]any{"push": map[string]any{"tags": []any{"v*"}}}), "the release must run only for a pushed v* tag")
	g.need(equal(at(release, "permissions"), map[string]any{"contents": "read"}), "the default permission must be contents: read")
	g.need(equal(at(release, "concurrency"), map[string]any{"group": "certum-signing", "cancel-in-progress": false}), "releases must queue on certum-signing, never cancel")
	g.need(at(config, "release", "draft") == true, "goreleaser must make a draft release")
	g.need(equal(at(config, "builds", 0, "hooks"), map[string]any{"post": []any{map[string]any{"cmd": goreleaserHook}}}), "the build's only post hook must sign Windows binaries outside snapshots")
	// The credential-bearing archive job must never publish containers.
	g.need(len(items(at(config, "dockers_v2"))) == 1, "expected one dockers_v2 entry")
	g.need(at(config, "dockers_v2", 0, "disable") == "{{ not .IsSnapshot }}", "goreleaser must build images only for snapshots")
	jobs := at(release, "jobs")
	g.need(equal(keys(jobs), []string{"image", "release", "signer", "verify-windows", "web"}), "unexpected jobs %v", keys(jobs))
	g.need(equal(at(jobs, "release", "needs"), []any{"web", "signer"}), "release must need web and signer")
	g.need(at(jobs, "verify-windows", "needs") == "release", "verify-windows must need release")
	g.need(equal(at(jobs, "image", "needs"), []any{"release", "verify-windows"}), "image must need release and verify-windows")
	permissions := map[string]any{
		"release": map[string]any{"contents": "write", "id-token": "write", "attestations": "write"},
		"image":   map[string]any{"contents": "read", "packages": "write", "id-token": "write", "attestations": "write"},
	}
	for _, name := range keys(jobs) {
		job := at(jobs, name)
		gate, runner, perms := repoOnly, "ubuntu-latest", permissions[name]
		if name == "image" {
			gate = imageGate
		}
		if name == "verify-windows" {
			runner = "windows-latest"
		}
		if perms == nil {
			perms = map[string]any{"contents": "read"}
		}
		g.need(at(job, "if") == gate, "job %s must run only %q", name, gate)
		g.need(at(job, "runs-on") == runner, "job %s must run on %s", name, runner)
		g.need(equal(at(job, "permissions"), perms), "job %s has the wrong permissions", name)
		for _, step := range items(at(job, "steps")) {
			g.checkPinned(step, anyAction)
		}
	}
	sign := g.one(stepsNamed(at(jobs, "release"), releaseSigningStep), "release signing step")
	g.need(at(sign, "run") == "goreleaser release --clean --parallelism=1", "the signing step must run goreleaser once, one build at a time")
	for _, name := range releaseSecrets {
		g.need(at(sign, "env", name) == expression("secrets."+name), "the signing step must take %s from its secret", name)
	}
	wantEnv := append(slices.Clone(releaseSecrets), "WEB_DIST", "IRIS_RELEASE_HELPER", "IRIS_WINDOWS_SIGNER", "XDG_RUNTIME_DIR")
	sort.Strings(wantEnv)
	g.need(equal(keys(at(sign, "env")), wantEnv), "the signing step's env must hold only its secrets and paths")
	helper := stepWithID(at(jobs, "release"), "helper")
	g.need(equal(at(helper, "env"), map[string]any{"CGO_ENABLED": "0"}), "the release helper must build without cgo")
	g.need(equal(lines(at(helper, "run")), []string{`go build -trimpath -o "$RUNNER_TEMP/iris-release-tools" ./cmd/iris-release-tools`, `echo "version=${GITHUB_REF_NAME#v}" >> "$GITHUB_OUTPUT"`}), "the release helper's build changed")
	outside := clone(release)
	for _, step := range items(at(outside, "jobs", "release", "steps")) {
		if text(at(step, "name")) == releaseSigningStep {
			delete(fields(step), "env")
		}
	}
	g.need(!mentionsSecrets(outside), "a secret is referenced outside the signing step's env")
	g.need(!anyKey(release, func(k string) bool { return k == "environment" || k == "continue-on-error" }), "no job or step may name an environment or continue on error")
	g.need(!anyText(release, func(s string) bool { return strings.Contains(s, "always()") }), "nothing may run with always()")
	signer := at(jobs, "signer", "steps")
	g.need(at(signer, 0, "with", "repository") == "Le-Syl21/ssign", "the signer must come from Le-Syl21/ssign")
	g.need(at(signer, 0, "with", "ref") == "f7ba7b2360a3e40bb8aa062d244d95c62a743b90", "the signer must be pinned to its reviewed commit")
	g.need(equal(lines(at(signer, 1, "run")), []string{"rustup toolchain install 1.99.1 --profile minimal", "cargo +1.99.1 build --release --locked", `sha256sum target/release/ssign >> "$GITHUB_STEP_SUMMARY"`}), "the signer's locked build changed")
	installs := stepsUsesPrefix(at(jobs, "release"), "goreleaser/")
	g.need(len(installs) > 0 && equal(at(installs, 0, "with"), map[string]any{"version": "v2.20.0", "install-only": true}), "goreleaser must be v2.20.0, installed only")
	for _, name := range []string{"web", "signer", "release"} {
		job := at(jobs, name)
		outputs := map[string]any{"artifact-id": expression("steps.upload.outputs.artifact-id")}
		if name == "release" {
			outputs["image-artifact-id"] = expression("steps.upload-image.outputs.artifact-id")
		}
		g.need(equal(at(job, "outputs"), outputs), "job %s has the wrong outputs", name)
		upload := stepWithID(job, "upload")
		g.need(strings.HasSuffix(text(at(upload, "with", "name")), runArtifact), "job %s's artifact must be named for its run", name)
		g.need(at(upload, "with", "retention-days") == 1, "job %s's artifact must be kept one day", name)
		g.need(at(upload, "with", "if-no-files-found") == "error", "job %s's upload must fail without files", name)
	}
	g.need(at(jobs, "web", "steps", -1, "with", "path") == "internal/webui/dist", "web must hand over only the console")
	g.need(at(signer, -1, "with", "path") == "ssign/target/release/ssign", "signer must hand over only the signer")
	g.need(equal(lines(at(jobs, "release", "steps", -1, "with", "path")), []string{"dist/iris_" + helperVer + "_windows_amd64.zip", "dist/iris_" + helperVer + "_windows_arm64.zip", "dist/checksums.txt"}), "release must hand over only the Windows archives and checksums")
	var downloads []any
	for _, step := range stepsUsing(at(jobs, "release"), "actions/download-artifact") {
		downloads = append(downloads, at(step, "with"))
	}
	g.need(equal(downloads, []any{
		map[string]any{"artifact-ids": expression("needs.web.outputs.artifact-id"), "path": expression("runner.temp") + "/webui-dist"},
		map[string]any{"artifact-ids": expression("needs.signer.outputs.artifact-id"), "path": expression("runner.temp") + "/signer"},
	}), "release must download exactly the console and the signer, by artifact id")
	checkImage(g, jobs)
	verify := at(jobs, "verify-windows")
	thumbprint := at(verify, "env", "EXPECTED_SIGNER_THUMBPRINT")
	g.need(thumbprint == signerThumbprint && at(proof, "jobs", "verify", "env", "EXPECTED_SIGNER_THUMBPRINT") == signerThumbprint, "both workflows must expect the reviewed signer thumbprint")
	g.need(at(verify, "steps", 0, "with", "ref") == expression("github.sha"), "verify-windows must check out the released commit")
	g.need(equal(at(verify, "steps", 2, "with"), map[string]any{"artifact-ids": expression("needs.release.outputs.artifact-id"), "path": expression("runner.temp") + "/windows-handoff"}), "verify-windows must download the release's handoff by id")
	archiveCheck := text(at(verify, "steps", 3, "run"))
	g.need(strings.Contains(archiveCheck, `verify-windows-archives "--version=$version" "--dir=$env:RUNNER_TEMP/windows-handoff" "--out=$env:RUNNER_TEMP/windows-verified"`), "verify-windows must check the archives")
	g.need(strings.Contains(archiveCheck, "GITHUB_STEP_SUMMARY"), "verify-windows must report the archive check")
	native := text(at(verify, "steps", 4, "run"))
	for _, policy := range []string{"foreach ($arch in 'amd64', 'arm64')", "windows-verified/$arch/iris.exe", "& $signtool verify /pa /all /v /tw $file", "if ($LASTEXITCODE -ne 0) { throw", "$signature.Status -ne 'Valid'", "$null -eq $signature.TimeStamperCertificate", "$signature.SignerCertificate.Thumbprint -ne $env:EXPECTED_SIGNER_THUMBPRINT", "GITHUB_STEP_SUMMARY"} {
		g.need(strings.Contains(native, policy), "verify-windows must keep %q", policy)
	}
	return g.err
}

// checkImage checks the image job: it takes the release's Linux archives by id, builds and pushes
// only the release target, moves latest only for a stable tag, and signs and attests by digest.
func checkImage(g *guard, jobs any) {
	image := at(jobs, "image")
	handoff := g.one(stepsWithID(at(jobs, "release"), "upload-image"), "image handoff upload")
	g.need(at(handoff, "uses") == "actions/upload-artifact@"+pins["actions/upload-artifact"], "the image handoff must use the pinned upload")
	g.need(equal(at(handoff, "with"), map[string]any{
		"name":              "image-release-" + runArtifact,
		"path":              "dist/iris_" + helperVer + "_linux_amd64.tar.gz\ndist/iris_" + helperVer + "_linux_arm64.tar.gz\ndist/checksums.txt\n",
		"if-no-files-found": "error", "retention-days": 1,
	}), "the image handoff must carry only the Linux archives and checksums")
	download := g.one(stepsUsing(image, "actions/download-artifact"), "image download")
	g.need(equal(at(download, "with"), map[string]any{"artifact-ids": expression("needs.release.outputs.image-artifact-id"), "path": expression("runner.temp") + "/image-release"}), "image must download the release's handoff by id")
	extract := g.one(stepsNamed(image, extractStep), "extract step")
	g.need(at(extract, "working-directory") == expression("runner.temp")+"/image-release", "extraction must run in the handoff directory")
	g.need(equal(at(extract, "env"), map[string]any{"RELEASE_TAG": expression("github.ref_name")}), "extraction must take the version from the tag")
	for _, policy := range []string{"set -euo pipefail", "version=${RELEASE_TAG#v}", "sha256sum --check --strict --ignore-missing checksums.txt", "for arch in amd64 arm64; do", `tar -xzf "iris_${version}_linux_${arch}.tar.gz" -C "linux/$arch" iris`, `cp "$GITHUB_WORKSPACE/Dockerfile" Dockerfile`} {
		g.need(strings.Contains(text(at(extract, "run")), policy), "extraction must keep %q", policy)
	}
	login := g.one(stepsUsing(image, "docker/login-action"), "registry login")
	g.need(equal(at(login, "with"), map[string]any{"registry": "ghcr.io", "username": expression("github.actor"), "password": expression("github.token")}), "the login must use the job's own token for ghcr.io")
	build := g.one(stepsUsing(image, "docker/build-push-action"), "image build")
	g.need(at(build, "id") == "image", "the image build's id must be image")
	g.need(at(build, "with", "context") == expression("runner.temp")+"/image-release", "the image must build from the released binaries")
	g.need(at(build, "with", "target") == "release", "the image must build the release target")
	g.need(at(build, "with", "platforms") == "linux/amd64,linux/arm64", "the image must be multi-arch")
	g.need(at(build, "with", "push") == true, "the image must be pushed")
	// Every release keeps its version tag; only refs without '-' move latest. The pinned action
	// ignores the empty second line for prereleases.
	g.need(equal(lines(at(build, "with", "tags")), []string{versionTag, stableLatest}), "the image tags must be the version, and latest only for a stable tag")
	g.need(at(build, "with", "provenance") == false, "the build's own provenance must stay off")
	cosign := g.one(stepsUsing(image, "sigstore/cosign-installer"), "cosign installer")
	g.need(index(image, login) < index(image, build), "the login must come before the build")
	g.need(index(image, cosign) > index(image, build), "cosign must be installed after the build")
	signImage := g.one(stepsNamed(image, signImageStep), "image signing step")
	g.need(equal(at(signImage, "env"), map[string]any{"IMAGE_DIGEST": expression("steps.image.outputs.digest")}), "the image must be signed by its digest")
	g.need(at(signImage, "run") == `cosign sign --yes "ghcr.io/mateusz-klatt/iris@$IMAGE_DIGEST"`, "the image signing command changed")
	attest := g.one(stepsUsing(image, "actions/attest-build-provenance"), "image attestation")
	g.need(equal(at(attest, "with"), map[string]any{"subject-name": "ghcr.io/mateusz-klatt/iris", "subject-digest": expression("steps.image.outputs.digest"), "push-to-registry": true}), "the attestation must name the image by digest and push it")
	// Prereleases still build, sign and attest; only the latest tag is conditional.
	for _, step := range []any{build, signImage, attest} {
		_, conditional := fields(step)["if"]
		g.need(!conditional, "the build, signature and attestation must run for every release")
	}
	g.need(index(image, build) < index(image, signImage) && index(image, signImage) < index(image, attest), "build, sign and attest must run in that order")
}

// releaseMutations are the weakenings of release.yml the release guard must reject.
func releaseMutations(release any) []mutation {
	m := []mutation{
		{[]any{"on"}, map[string]any{"workflow_dispatch": nil}},
		{[]any{"concurrency", "group"}, "other"},
		{[]any{"concurrency", "cancel-in-progress"}, true},
		{[]any{"permissions"}, map[string]any{"contents": "write"}},
		{[]any{"environment"}, "signing"},
		{[]any{"env"}, map[string]any{"LEAK": expression("secrets.CERTUM_OTP")}},
		{[]any{"jobs", "web", "if"}, "always()"},
		{[]any{"jobs", "signer", "if"}, "always()"},
		{[]any{"jobs", "release", "if"}, "always()"},
		{[]any{"jobs", "verify-windows", "if"}, "always()"},
		{[]any{"jobs", "web", "permissions"}, map[string]any{"contents": "write"}},
		{[]any{"jobs", "verify-windows", "permissions"}, map[string]any{"contents": "write"}},
		{[]any{"jobs", "release", "continue-on-error"}, true},
		{[]any{"jobs", "release", "env"}, map[string]any{"LEAK": expression("toJSON(secrets)")}},
		{[]any{"jobs", "signer", "steps", 0, "with", "ref"}, "main"},
		{[]any{"jobs", "signer", "steps", 1, "run"}, "cargo +stable build --release"},
		{[]any{"jobs", "release", "needs"}, []any{"web"}},
		{[]any{"jobs", "verify-windows", "needs"}, "signer"},
		{[]any{"jobs", "release", "steps", 0, "uses"}, "actions/checkout@main"},
		{[]any{"jobs", "release", "steps", 1, "with", "artifact-ids"}, "1,2"},
		{[]any{"jobs", "release", "steps", -1, "with", "path"}, "dist/*"},
		{[]any{"jobs", "verify-windows", "env", "EXPECTED_SIGNER_THUMBPRINT"}, "OTHER"},
		{[]any{"jobs", "verify-windows", "steps", -1, "run"}, "& $signtool verify /pa $file"},
	}
	for i, step := range items(at(release, "jobs", "release", "steps")) {
		if text(at(step, "name")) == releaseSigningStep {
			m = append(m, mutation{[]any{"jobs", "release", "steps", i, "run"}, "goreleaser release --clean"})
			break
		}
	}
	native := text(at(release, "jobs", "verify-windows", "steps", -1, "run"))
	for _, r := range [][2]string{
		{"/pa /all /v /tw", "/pa /all /v"},
		{"$LASTEXITCODE -ne 0", "$LASTEXITCODE -eq 1"},
		{"$null -eq $signature.TimeStamperCertificate", "$false"},
		{"$signature.Status -ne 'Valid'", "$false"},
		{"$signature.SignerCertificate.Thumbprint -ne $env:EXPECTED_SIGNER_THUMBPRINT", "$false"},
	} {
		m = append(m, mutation{[]any{"jobs", "verify-windows", "steps", -1, "run"}, strings.ReplaceAll(native, r[0], r[1])})
	}
	// Only the publishing job gets registry write access, never a workflow default.
	m = append(m, mutation{[]any{"permissions"}, map[string]any{"contents": "read", "packages": "write"}})
	for _, name := range []string{"web", "signer", "release", "verify-windows"} {
		m = append(m, mutation{[]any{"jobs", name, "permissions"}, with(at(release, "jobs", name, "permissions"), "packages", "write")})
	}
	m = append(m, imageMutations(release)...)
	for i, step := range items(at(release, "jobs", "release", "steps")) {
		if text(at(step, "id")) == "upload-image" {
			m = append(m, mutation{[]any{"jobs", "release", "steps", i, "with", "path"}, "dist/*"})
			break
		}
	}
	return m
}

// imageMutations are the weakenings of the image job.
func imageMutations(release any) []mutation {
	path := func(rest ...any) []any { return append([]any{"jobs", "image"}, rest...) }
	image := at(release, "jobs", "image")
	permissions := fields(at(image, "permissions"))
	m := []mutation{
		{path("if"), "github.repository == 'mateusz-klatt/iris'"},
		{path("if"), "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')"},
		{path("if"), "github.repository == 'mateusz-klatt/iris' && startsWith(github.ref, 'refs/tags/v')"},
		{path("needs"), []any{"release"}},
		{path("permissions"), with(permissions, "contents", "write")},
		{path("env"), map[string]any{"LEAK": expression("secrets.CERTUM_OTP")}},
		{path("continue-on-error"), true},
		{[]any{"jobs", "release", "outputs", "image-artifact-id"}, expression("steps.upload.outputs.artifact-id")},
	}
	for _, permission := range keys(permissions) {
		narrowed := map[string]any{}
		for k, v := range permissions {
			if k != permission {
				narrowed[k] = v
			}
		}
		m = append(m, mutation{path("permissions"), narrowed})
	}
	steps := items(at(image, "steps"))
	for i, step := range steps {
		uses, name := text(at(step, "uses")), text(at(step, "name"))
		step := func(rest ...any) []any { return path(append([]any{"steps", i}, rest...)...) }
		if _, ok := fields(steps[i])["uses"]; ok {
			action, _, _ := strings.Cut(uses, "@")
			m = append(m, mutation{step("uses"), action + "@main"})
		}
		switch {
		case strings.HasPrefix(uses, "actions/checkout@"):
			m = append(m, mutation{step("with", "persist-credentials"), true})
		case strings.HasPrefix(uses, "actions/download-artifact@"):
			m = append(m, mutation{step("with", "artifact-ids"), "1,2"})
		case strings.HasPrefix(uses, "docker/login-action@"):
			m = append(m, mutation{step("with", "registry"), "docker.io"}, mutation{step("with", "password"), expression("secrets.CERTUM_OTP")})
		case strings.HasPrefix(uses, "docker/build-push-action@"):
			for _, kv := range []struct {
				key   string
				value any
			}{{"context", "."}, {"target", "deploy"}, {"platforms", "linux/amd64"}, {"push", false}, {"tags", "ghcr.io/mateusz-klatt/iris:latest"}, {"provenance", true}} {
				m = append(m, mutation{step("with", kv.key), kv.value})
			}
			// Reject prereleases moving latest, stable releases omitting it, and a reversed
			// condition that publishes latest only for prereleases.
			for _, tags := range []string{
				versionTag + "\nghcr.io/mateusz-klatt/iris:latest\n",
				versionTag + "\n",
				versionTag + "\n" + strings.ReplaceAll(stableLatest, "!contains", "contains") + "\n",
			} {
				m = append(m, mutation{step("with", "tags"), tags})
			}
		case strings.HasPrefix(uses, "actions/attest-build-provenance@"):
			for _, kv := range []struct {
				key   string
				value any
			}{{"subject-name", "ghcr.io/other/image"}, {"subject-digest", expression("github.sha")}, {"push-to-registry", false}} {
				m = append(m, mutation{step("with", kv.key), kv.value})
			}
		}
		switch name {
		case extractStep:
			m = append(m, mutation{step("run"), strings.ReplaceAll(text(at(steps[i], "run")), "sha256sum --check --strict --ignore-missing checksums.txt", "true")}, mutation{step("env", "RELEASE_TAG"), expression("github.event.pull_request.title")})
		case signImageStep:
			m = append(m, mutation{step("env", "IMAGE_DIGEST"), expression("github.sha")}, mutation{step("run"), "cosign sign --yes ghcr.io/mateusz-klatt/iris:latest"})
		}
		if name == signImageStep || strings.HasPrefix(uses, "actions/attest-build-provenance@") {
			without := make([]any, 0, len(steps)-1)
			for j, other := range steps {
				if j != i {
					without = append(without, clone(other))
				}
			}
			m = append(m, mutation{path("steps"), without})
		}
		if name == signImageStep || strings.HasPrefix(uses, "docker/build-push-action@") || strings.HasPrefix(uses, "actions/attest-build-provenance@") {
			m = append(m, mutation{step("if"), "!contains(github.ref_name, '-')"})
		}
	}
	return m
}

// configMutations are the weakenings of .goreleaser.yaml the release guard must reject.
func configMutations() []mutation {
	return []mutation{
		{[]any{"release", "draft"}, false},
		{[]any{"builds", 0, "hooks", "post", 0, "cmd"}, strings.ReplaceAll(goreleaserHook, "(not .IsSnapshot)", "true")},
		{[]any{"dockers_v2", 0, "disable"}, false},
	}
}
