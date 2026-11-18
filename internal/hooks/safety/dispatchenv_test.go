package safety

import (
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

type tamperCase struct {
	name, cmd, agentID, dispatchID string
	want                           core.Decision
}

var tamperCases = []tamperCase{
	{"assign empty id", `HARNESS_DISPATCH_ID= claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"env -u id", `env -u HARNESS_DISPATCH_ID claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"unset permissions", `unset HARNESS_DISPATCH_PERMISSIONS; claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign max depth", `HARNESS_DISPATCH_MAX_DEPTH=0 claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign permissions", `HARNESS_DISPATCH_PERMISSIONS='{}' claude-code codex ask x`, "a1", "", core.Block},
	{"export depth", `export HARNESS_DISPATCH_DEPTH=0`, "a1", "", core.Block},
	{"env -i before claude-code", `env -i PATH=/usr/bin claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"dispatch leaf unsets", `unset HARNESS_DISPATCH_ID`, "", "d1", core.Block},
	{"env -i before a vendor launcher", `env -i PATH=/usr/bin codex exec x`, "a1", "", core.Block},
	{"env -i before opencode", `env -i opencode run x`, "a1", "", core.Block},
	{"assign after another assignment", `FOO=1 HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"assign through env after -u", `env -u FOO HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"unset inside bash -c", `bash -c "unset HARNESS_DISPATCH_ID; claude-code agents ask x"`, "a1", "", core.Block},
	{"assign inside sh -c", `sh -c 'HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x'`, "a1", "", core.Block},
	{"exec env -u id", `exec env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env -u id", `nohup env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time env -u id", `time env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"command env -u id", `command env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"exec env -i launcher", `exec env -i claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env assigns depth", `nohup env HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x`, "a1", "", core.Block},
	{"sudo env -u id", `sudo env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"absolute env -u id", `/usr/bin/env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time go test", `time go test ./...`, "a1", "", core.Allow},
	{"nohup dispatch", `nohup claude-code agents ask --agent codex "x" &`, "a1", "", core.Allow},
	{"exec env unrelated assign", `exec env FOO=1 claude-code agents ask x`, "a1", "", core.Allow},
	{"command -v env", `command -v env`, "a1", "", core.Allow},
	{"time env piped to sort", `time env | sort`, "a1", "", core.Allow},
	{"nice make", `nice -n 10 make test`, "a1", "", core.Allow},
	{"text only", `echo "exec env -u HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"argument is no assignment", `go test ./... -args HARNESS_DISPATCH_DEPTH=2`, "a1", "", core.Allow},
	{"plain dispatch", `claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"echo id", `echo $HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"printenv depth", `printenv HARNESS_DISPATCH_DEPTH`, "a1", "", core.Allow},
	{"unrelated assign", `FOO=1 claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"env -i other command", `env -i PATH=/usr/bin ls`, "a1", "", core.Allow},
	{"assign before env -u", `A=1 env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"sudo env -u", `sudo env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"xargs env -u", `echo x | xargs env -u HARNESS_DISPATCH_ID claude-code agents ask`, "a1", "", core.Block},
	{"find -exec env -u", `find . -exec env -u HARNESS_DISPATCH_ID claude-code agents ask {} \;`, "a1", "", core.Block},
	{"exec -a env -u", `exec -a x env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"then env -u", `if true; then env -u HARNESS_DISPATCH_ID claude-code agents ask x; fi`, "a1", "", core.Block},
	{"else env -u", `if false; then true; else env -u HARNESS_DISPATCH_ID claude-code agents ask x; fi`, "a1", "", core.Block},
	{"do env -u", `for i in 1; do env -u HARNESS_DISPATCH_ID claude-code agents ask x; done`, "a1", "", core.Block},
	{"export -n", `export -n HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"declare +x", `declare +x HARNESS_DISPATCH_DEPTH`, "a1", "", core.Block},
	{"typeset +x", `typeset +x HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"env -i sh -c claude-code", `env -i sh -c 'claude-code agents ask x'`, "a1", "", core.Block},
	{"env -i claude-code", `env -i claude-code agents ask x`, "a1", "", core.Block},
	{"env dash claude-code", `env - claude-code agents ask x`, "a1", "", core.Block},
	{"env -u other var", `env -u SOME_OTHER_VAR claude-code agents ask x`, "a1", "", core.Allow},
	{"env -i make", `env -i make`, "a1", "", core.Allow},
	{"find exec grep id", `find . -exec grep HARNESS_DISPATCH_ID {} \;`, "a1", "", core.Allow},
	{"grep -rn id", `grep -rn HARNESS_DISPATCH_ID .`, "a1", "", core.Allow},
	{"export other", `export FOO=1`, "a1", "", core.Allow},
	{"commit message mentions unset", `git commit -m 'docs: unset HARNESS_DISPATCH_ID is blocked'`, "a1", "", core.Allow},
	{"heredoc mentions unset", "cat > notes.txt <<'EOF'\nunset HARNESS_DISPATCH_ID\nenv -u HARNESS_DISPATCH_ID x\nEOF", "a1", "", core.Allow},
	{"builtin unset", `builtin unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"command unset", `command unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"unquoted eval unset", `eval unset HARNESS_DISPATCH_ID`, "a1", "", core.Block},
	{"brace list unset", `unset HARNESS_DISPATCH_{ID,DEPTH}`, "a1", "", core.Block},
	{"brace list env -u", `env -u HARNESS_DISPATCH_{ID,DEPTH} claude-code agents ask x`, "a1", "", core.Block},
	{"su -c unset", `su -c 'unset HARNESS_DISPATCH_ID; claude-code agents ask x' root`, "a1", "", core.Block},
	{"script -qc unset", `script -qc 'unset HARNESS_DISPATCH_ID; claude-code agents ask x' /dev/null`, "a1", "", core.Block},
	{"indirect unset", `N=HARNESS_DISPATCH_ID; unset $N`, "a1", "", core.Block},
	{"indirect quoted export -n", `N="HARNESS_DISPATCH_DEPTH"; export -n ${N}`, "a1", "", core.Block},
	{"indirect env -u", `N=HARNESS_DISPATCH_ID && env -u $N claude-code agents ask x`, "a1", "", core.Block},
	{"heredoc into bash", "bash <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc into sh -s", "sh -s <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc piped to sh", "cat <<EOF | sh\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"heredoc written then run", "cat > s.sh <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nbash s.sh", "a1", "", core.Block},
	{"commit heredoc body", "git commit -F - <<'EOF'\nfix: x\n\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"bare cat heredoc body", "cat <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"commit message substitution heredoc", "git commit -m \"$(cat <<'EOF'\nfix: x\n\nunset HARNESS_DISPATCH_ID\nEOF\n)\"", "a1", "", core.Allow},
	{"su other user", `su - otheruser`, "a1", "", core.Allow},
	{"script no -c", `script -q /dev/null ls`, "a1", "", core.Allow},
	{"alias unused", `N=HARNESS_DISPATCH_ID; echo $N`, "a1", "", core.Allow},
	{"env -u other command", `env -u FOO claude-code agents status`, "a1", "", core.Allow},
	{"echo unset", `echo unset HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"command -v unset", `command -v unset`, "a1", "", core.Allow},
	{"main session sets depth", `HARNESS_DISPATCH_MAX_DEPTH=5 claude-code agents ask --agent codex x`, "", "", core.Allow},
	{"tee then source", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nsource s", "a1", "", core.Block},
	{"tee then dot", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\n. s", "a1", "", core.Block},
	{"tee then sh", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nsh s", "a1", "", core.Block},
	{"tee then bash <", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nbash < s", "a1", "", core.Block},
	{"tee then env bash", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nenv bash s", "a1", "", core.Block},
	{"tee then sh <s", "tee s <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\nsh <s", "a1", "", core.Block},
	{"here-string to bash", `bash <<< "unset HARNESS_DISPATCH_ID"`, "a1", "", core.Block},
	{"eval of printf", `eval "$(printf 'unset HARNESS_DISPATCH_ID')"`, "a1", "", core.Block},
	{"escaped name", `unset HARNESS\_DISPATCH_ID`, "a1", "", core.Block},
	{"split-quoted name", `unset HARNESS_DISPATCH_"ID"`, "a1", "", core.Block},
	{"quoted name", `unset 'HARNESS_DISPATCH_ID'`, "a1", "", core.Block},
	{"printf piped to bash", `printf 'unset HARNESS_DISPATCH_ID\n' | bash`, "a1", "", core.Block},
	{"parallel env -u", `parallel env -u HARNESS_DISPATCH_ID true`, "a1", "", core.Block},
	{"xargs sh -c from printf", `printf '%s\n' 'unset HARNESS_DISPATCH_ID' | xargs -I CMD sh -c CMD`, "a1", "", core.Block},
	{"python heredoc", "python3 - <<'PY'\nimport os; os.system('unset HARNESS_DISPATCH_ID')\nPY", "a1", "", core.Block},
	{"ssh env -u", `ssh h 'env -u HARNESS_DISPATCH_ID x'`, "a1", "", core.Block},
	{"phrase in echo beside an evaluator", `sh build.sh && echo "unset HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"inert note then python", "cat > note.md <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\npython3 -m pytest", "a1", "", core.Allow},
	{"commit body then bash", "git commit -F - <<'EOF'\nfix: x\n\nunset HARNESS_DISPATCH_ID is refused\nEOF\nbash post.sh",
		"a1", "", core.Allow},
	{"comment after bash -c", `bash -c 'echo hi' # unset HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"node then echo", `node script.js; echo "unset HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"commit -m then bash", `git commit -m "docs: unset HARNESS_DISPATCH_ID guidance" && bash build.sh`, "a1", "", core.Allow},
	{"grep then python", `grep -rn 'unset HARNESS_DISPATCH_ID' rules/ | head; python3 x.py`, "a1", "", core.Allow},
	{"rg assignment then node", `rg -n 'HARNESS_DISPATCH_ID=' internal | head && node a.js`, "a1", "", core.Allow},
	{"value names a dispatch var", `FOO=HARNESS_DISPATCH_ID_X bash run.sh`, "a1", "", core.Allow},
	{"ssh grep", `ssh host 'grep -rn HARNESS_DISPATCH_ID .'`, "a1", "", core.Allow},
	{"bash then echo the id", `bash build.sh; echo $HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"commit mentions assignment", `git commit -m "docs: mention HARNESS_DISPATCH_ID=" && make test`, "a1", "", core.Allow},
	{"container shell", `docker run img sh -c 'unset HARNESS_DISPATCH_ID'`, "a1", "", core.Allow},
	{"inert heredoc beside sh -c", "sh -c 'echo hi'; cat <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"alias into bash -c", `N=HARNESS_DISPATCH_ID; bash -c "unset $N"`, "a1", "", core.Block},
	{"echo script run by path", `echo 'unset HARNESS_DISPATCH_ID' > s.sh && chmod +x s.sh && ./s.sh`, "a1", "", core.Block},
	{"heredoc script run by path", "cat > s.sh <<EOF\nunset HARNESS_DISPATCH_ID\nEOF\n./s.sh", "a1", "", core.Block},
	{"absolute script path", "cat > /tmp/s.sh <<EOF\nunset HARNESS_DISPATCH_ID\nEOF\nchmod +x /tmp/s.sh && /tmp/s.sh", "a1", "",
		core.Block},
	{"versioned python", `python3.12 -c 'import os; os.system("unset HARNESS_DISPATCH_ID")'`, "a1", "", core.Block},
	{"pypy3", `pypy3 -c 'import os; os.system("unset HARNESS_DISPATCH_ID")'`, "a1", "", core.Block},
	{"bun", `bun -e 'require("child_process").execSync("unset HARNESS_DISPATCH_ID")'`, "a1", "", core.Block},
	{"deno", `deno eval 'Deno.run({cmd: ["sh", "-c", "unset HARNESS_DISPATCH_ID"]})'`, "a1", "", core.Block},
	{"php", `php -r 'system("unset HARNESS_DISPATCH_ID");'`, "a1", "", core.Block},
	{"lua", `lua -e 'os.execute("unset HARNESS_DISPATCH_ID")'`, "a1", "", core.Block},
	{"fish", `fish -c 'set -e HARNESS_DISPATCH_ID; env -u HARNESS_DISPATCH_ID x'`, "a1", "", core.Block},
	{"busybox sh", `busybox sh -c 'unset HARNESS_DISPATCH_ID'`, "a1", "", core.Block},
	{"nsenter sh", `nsenter -t 1 sh -c 'unset HARNESS_DISPATCH_ID'`, "a1", "", core.Block},
	{"pipe through sort into bash", `printf 'unset HARNESS_DISPATCH_ID' | sort | bash`, "a1", "", core.Block},
	{"substitution as command", `$(printf 'unset HARNESS_DISPATCH_ID')`, "a1", "", core.Block},
	{"env --unset=", `env --unset=HARNESS_DISPATCH_ID x`, "a1", "", core.Block},
	{"env -uNAME", `env -uHARNESS_DISPATCH_ID x`, "a1", "", core.Block},
	{"heredoc with spaced delimiter", "cat << EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"sed -n reader", "sed -n p <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"keyword before reader", "if true; then cat <<EOF\nunset HARNESS_DISPATCH_ID\nEOF\nfi", "a1", "", core.Allow},
	{"sort filter", "sort <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"jq filter", "jq . <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"tr filter", "tr a b <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"base64 filter", "base64 <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"pbcopy filter", "pbcopy <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"gh body file", "gh pr create --title t --body-file - <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"awk may run its input", "awk '{system($0)}' <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"mysql may shell out", "mysql <<EOF\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Block},
	{"tee note never run", "tee notes.md <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"cat note then git add", "cat > notes.md <<'EOF'\nunset HARNESS_DISPATCH_ID\nEOF\ngit add notes.md", "a1", "", core.Allow},
	{"heredoc to wc", "cat <<EOF | wc -l\nunset HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"commit -F env -u", "git commit -F - <<'EOF'\nfix: env -u HARNESS_DISPATCH_ID\nEOF", "a1", "", core.Allow},
	{"xargs grep phrase", `xargs grep "unset HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"xargs -- grep phrase", `xargs -- grep 'unset HARNESS_DISPATCH_ID'`, "a1", "", core.Allow},
	{"find exec grep", `find . -name '*.go' -exec grep -l HARNESS_DISPATCH {} \;`, "a1", "", core.Allow},
	{"bash -c echo", `bash -c 'echo hi'`, "a1", "", core.Allow},
	{"sh script", `sh build.sh`, "a1", "", core.Allow},
	{"bash reads the id", `bash -c 'echo $HARNESS_DISPATCH_ID'`, "a1", "", core.Allow},
	{"parallel grep", `parallel grep HARNESS_DISPATCH_ID ::: a b`, "a1", "", core.Allow},
}

// A leaf may not assign, export, unset or clear a HARNESS_DISPATCH_* variable; the main session may.
func TestDispatchTamper(t *testing.T) {
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	for _, c := range tamperCases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HARNESS_DISPATCH_ID", c.dispatchID)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": "/tmp", "agent_id": c.agentID,
				"tool_input": map[string]string{"command": c.cmd}})
			if r := Scan(core.ParseInput(bytes.NewReader(payload))); r.Decision != c.want {
				t.Fatalf("decision %v, want %v: %s", r.Decision, c.want, r.Reason)
			}
		})
	}
}

// sudo is blocked by its own rule; the tamper check alone must not read an ordinary sudo command as a tamper.
func TestTamperNameIgnoresPlainSudo(t *testing.T) {
	if name := tamperName("sudo ls"); name != "" {
		t.Errorf("tamperName(sudo ls) = %q, want none", name)
	}
}

type roleCase struct {
	cmd, cwd, kube string // CFG in cmd and cwd is the config dir; kube "" leaves the kubeconfig unreadable
	main, leaf     core.Decision
}

const devKube = "apiVersion: v1\ncurrent-context: dev-cluster\n"

var roleCases = []roleCase{
	{"claude-code vault get --key --secret", "", devKube, core.Ask, core.Block},
	{"rtk claude-code vault get --secret --key", "", devKube, core.Ask, core.Block},
	{"env X=1 claude-code vault get --key --secret | age -e", "", devKube, core.Ask, core.Block},
	{"sh -c 'claude-code vault get -key -secret > k'", "", devKube, core.Ask, core.Block},
	{"K=$(claude-code vault get --key --secret=true)", "", devKube, core.Ask, core.Block},
	{"go run ./cmd/claude-code vault get --key --secret", "", devKube, core.Ask, core.Block},
	{"claude-code vault get --key", "", devKube, core.Allow, core.Allow},
	{"claude-code vault get --key --secret=false", "", devKube, core.Allow, core.Allow},
	{"claude-code vault get GITHUB_TOKEN", "", devKube, core.Allow, core.Allow},
	{"claude-code vault get --secret GITHUB_TOKEN", "", devKube, core.Allow, core.Block},
	{"T=$(claude-code resolve --profile dev --secret)", "", devKube, core.Allow, core.Block},
	{"claude-code gateway proxy --client-token ed --secret", "", devKube, core.Allow, core.Block},
	{"claude-code network tailscale --key --secret", "", devKube, core.Allow, core.Block},
	{"claude-code vault get --secret --export A B", "", devKube, core.Allow, core.Block},
	{"claude-code vault get A -- --secret", "", devKube, core.Allow, core.Allow},
	{`claude-code agents ask --agent codex review the --secret flag`, "", devKube, core.Allow, core.Allow},
	{`grep -- "--secret" rules/`, "", devKube, core.Allow, core.Allow},
	{`grep "vault get --key" rules/`, "", devKube, core.Allow, core.Allow},
	{`git commit -m "feat: claude-code vault get --key"`, "", devKube, core.Allow, core.Allow},
	{"kubectl -n prod exec pod -- ls", "", devKube, core.Ask, core.Block},
	{"kubectl exec -n prod pod -- ls", "", devKube, core.Ask, core.Block},
	{"kubectl --context=prod delete pod x", "", devKube, core.Ask, core.Block},
	{"kubectl --context prod-east delete pod x", "", devKube, core.Ask, core.Block},
	{"oc rsh --namespace=ns-production pod", "", devKube, core.Ask, core.Block},
	{"rtk kubectl exec -n my-prod-cluster pod -- ls", "", devKube, core.Ask, core.Block},
	{"env A=1 oc delete pod x -n prd", "", devKube, core.Ask, core.Block},
	{"sh -c 'kubectl delete pod x --context prod'", "", devKube, core.Ask, core.Block},
	{"make login ENV=prod && oc exec pod -- ls", "/tmp", devKube, core.Ask, core.Block},
	{"kubectl --kubeconfig ~/.kube/prod.yaml exec pod -- ls", "", devKube, core.Ask, core.Block},
	{"kubectl exec pod -- ls", "", "current-context: cluster-prod\n", core.Ask, core.Block},
	{"oc delete pod x", "", "", core.Ask, core.Block},
	{"kubectl delete pod x", "", devKube, core.Allow, core.Allow},
	{"kubectl exec -n product-catalog pod -- ls", "", devKube, core.Allow, core.Allow},
	{"kubectl delete pod x -n reproduce", "", devKube, core.Allow, core.Allow},
	{"kubectl --context dev delete pod x", "", "current-context: prod\n", core.Allow, core.Allow},
	{"kubectl get pods -n prod", "", devKube, core.Allow, core.Allow},
	{"oc logs -n prod pod", "", "", core.Allow, core.Allow},
	{`grep "kubectl delete" docs/`, "", "", core.Allow, core.Allow},
	{`git commit -m "fix: kubectl exec -n prod"`, "", "", core.Allow, core.Allow},
	{"make audit", "", devKube, core.Allow, core.Block},
	{"make install", "", devKube, core.Allow, core.Block},
	{"make fmt", "", devKube, core.Allow, core.Block},
	{"make fmt WHAT=md AGENT=off", "", devKube, core.Allow, core.Block},
	{"make -C CFG audit FORCE=1", "/tmp", devKube, core.Allow, core.Block},
	{"rtk make install", "", devKube, core.Allow, core.Block},
	{"env FORCE=1 make audit", "", devKube, core.Allow, core.Block},
	{"sh -c 'make -j4 install'", "", devKube, core.Allow, core.Block},
	{"make lint", "", devKube, core.Allow, core.Allow},
	{"make lint WHAT=go", "", devKube, core.Allow, core.Allow},
	{"make test", "", devKube, core.Allow, core.Allow},
	{"make auditx", "", devKube, core.Allow, core.Allow},
	{"make fmtx", "", devKube, core.Allow, core.Allow},
	{"make install-tools", "", devKube, core.Allow, core.Allow},
	{`grep -n "make install" README.md`, "", devKube, core.Allow, core.Allow},
	{`grep -n "make fmt" Makefile`, "", devKube, core.Allow, core.Allow},
	{"make audit", "/tmp", devKube, core.Allow, core.Allow},
	{"make -C /tmp audit", "", devKube, core.Allow, core.Allow},
	{"make build", "", devKube, core.Allow, core.Block},
	{"make SKIP_AUDIT=1 build", "", devKube, core.Allow, core.Block},
	{"make verify", "", devKube, core.Allow, core.Block},
	{"make bootstrap", "", devKube, core.Allow, core.Block},
	{"make adopt", "", devKube, core.Allow, core.Block},
	{"gmake install", "", devKube, core.Allow, core.Block},
	{"make lint install", "", devKube, core.Allow, core.Block},
	{"make -f Makefile audit", "", devKube, core.Allow, core.Block},
	{"make lint-go", "", devKube, core.Allow, core.Block},
	{"make -n audit", "", devKube, core.Allow, core.Block},
	{"make lint-go CHECK=1", "", devKube, core.Allow, core.Allow},
	{"make compile", "", devKube, core.Allow, core.Allow},
	{"make install", "WT", devKube, core.Allow, core.Block},
	{"make -C WT build", "/tmp", devKube, core.Allow, core.Block},
	{"make audit", "WT", devKube, core.Allow, core.Allow},
	{"make fmt", "WT", devKube, core.Allow, core.Allow},
	{"kubectl -nprod delete pod x", "", devKube, core.Ask, core.Block},
	{"kubectl --request-timeout 5s delete pod x -n prod", "", devKube, core.Ask, core.Block},
	{"kubectl --v 6 delete pod x -n prod", "", devKube, core.Ask, core.Block},
	{"kubectl --context prod-eu -n x exec pod -- ls", "", devKube, core.Ask, core.Block},
	{"kubectl -n x --as y delete pod z", "", "", core.Ask, core.Block},
	{"KUBECONFIG=/path/prod.yaml kubectl delete pod x", "", devKube, core.Ask, core.Block},
	{"export KUBECONFIG=/p/prod.yaml; kubectl delete pod x", "", devKube, core.Ask, core.Block},
	{"kubectl delete namespace prod", "", devKube, core.Ask, core.Block},
	{"kubectl delete ns/prod", "", devKube, core.Ask, core.Block},
	{"oc delete project prod", "", devKube, core.Ask, core.Block},
	{"kubectl config use-context prod && kubectl delete pod x", "", devKube, core.Ask, core.Block},
	{"kubectl --server https://api.prod.example.com delete pod x", "", devKube, core.Ask, core.Block},
	{"kubectl exec -n prod-like-test pod -- ls", "", devKube, core.Ask, core.Block},
	{"kubectl exec pod -- env ENV=prod true", "", devKube, core.Allow, core.Allow},
	{"kubectl delete namespace staging", "", devKube, core.Allow, core.Allow},
	{"KUBECONFIG=/kube/config kubectl delete pod x", "", devKube, core.Allow, core.Allow},
	{"KUBECONFIG=/kube/other kubectl delete pod x", "", devKube, core.Ask, core.Block},
	{"claude-code --remote h vault get --key --secret", "", devKube, core.Ask, core.Block},
	{"claude-code --host h vault get --key --secret", "", devKube, core.Ask, core.Block},
	{"claude-code --host=h vault get --key --secret", "", devKube, core.Ask, core.Block},
	{"claude-code --host h vault get --secret NAME", "", devKube, core.Allow, core.Block},
	{"claude-code --host h vault get NAME", "", devKube, core.Allow, core.Allow},
}

// The main session is asked (or keeps its verdict); a subagent and a CLI-dispatched agent are refused.
func TestRoleRules(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir() // wt is a linked worktree of the harness repo
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+cfg+"/.git/worktrees/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	t.Setenv("KUBECONFIG", "/kube/config:/other")
	for _, c := range roleCases {
		kube := c.kube
		swapKubeconfig(t, func(p string) ([]byte, error) {
			if kube == "" || p != "/kube/config" {
				return nil, os.ErrNotExist
			}
			return []byte(kube), nil
		})
		at := strings.NewReplacer("CFG", cfg, "WT", wt)
		cmd, cwd := at.Replace(c.cmd), at.Replace(cmp.Or(c.cwd, cfg))
		for _, role := range []struct{ agentID, dispatchID string }{{"", ""}, {"a1", ""}, {"", "d1"}} {
			t.Setenv("HARNESS_DISPATCH_ID", role.dispatchID)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cwd, "agent_id": role.agentID,
				"tool_input": map[string]string{"command": cmd}})
			want := c.leaf
			if role.agentID == "" && role.dispatchID == "" {
				want = c.main
			}
			if r := Scan(core.ParseInput(bytes.NewReader(payload))); r.Decision != want {
				t.Errorf("%q as %+v: decision %v, want %v: %s", cmd, role, r.Decision, want, r.Reason)
			}
		}
	}
}

// On an agent with no ask decision the main session is blocked, and the message names the routes that ask.
func TestMainOnlyRouteWithoutAsk(t *testing.T) {
	t.Setenv("HARNESS_DISPATCH_ID", "")
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": "/tmp",
		"tool_input": map[string]string{"command": "claude-code vault get --key --secret"}})
	r := Scan(core.ParseInputFor(bytes.NewReader(payload), "copilot"))
	if r.Decision != core.Block || !strings.Contains(r.Reason, "from a Claude session") || !strings.Contains(r.Reason, "terminal") {
		t.Errorf("copilot main session: decision %v, want a block naming the Claude and terminal routes: %s", r.Decision, r.Reason)
	}
}

func swapKubeconfig(t *testing.T, fn func(string) ([]byte, error)) {
	old := readKubeconfig
	readKubeconfig = fn
	t.Cleanup(func() { readKubeconfig = old })
}
