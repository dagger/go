#!/usr/bin/env bash
set -euo pipefail

module_source=$(cd "$(dirname "$0")/.." && pwd)
probe=$(mktemp -d)
dagger_bin=${DAGGER_BIN:-dagger}
trap 'result=$?; if [ "$result" -eq 0 ] && [ "${KEEP_ULTRABATCH_FIXTURE:-0}" != 1 ]; then rm -rf "$probe"; else printf "Fixture and logs: %s\n" "$probe"; fi' EXIT

python3 - "$probe" "$module_source" <<'PY'
import json, sys
from pathlib import Path
root=Path(sys.argv[1])
(root/'dagger.toml').write_text('[modules.go]\nsource = '+json.dumps(sys.argv[2])+'\n')
for module in ['m1','m2']:
    p=root/module;p.mkdir()
    (p/'go.mod').write_text(f'module example.com/{module}\n\ngo 1.26.1\n')
    for package in ['p1','p2','unused']:
        p=root/module/package
        p.mkdir()
        (p/'tests_test.go').write_text('package '+package+'\nimport "testing"\nfunc TestFoo(t *testing.T) {}\nfunc TestBar(t *testing.T) {}\nfunc TestOther(t *testing.T) { t.Fatal("excluded test ran") }\n')
PY
git -C "$probe" init -q
cd "$probe"
selection=(--go-package=m1/p1 --go-package=m1/p2 --go-package=m2/p1 --go-package=m2/p2 --go-test=TestFoo --go-test=TestBar)
"$dagger_bin" check "${selection[@]}" >pass.stdout 2>pass.stderr
grep -q '8 passed' pass.stderr

python3 - <<'PY'
from pathlib import Path
p=Path('m1/p1/tests_test.go')
p.write_text(p.read_text().replace('func TestFoo(t *testing.T) {}','func TestFoo(t *testing.T) { t.Fatal("selected failure") }'))
PY
if "$dagger_bin" check "${selection[@]}" >fail.stdout 2>fail.stderr; then
    echo 'Expected the selected test to fail' >&2; exit 1
fi
grep -q '7 passed' fail.stderr
grep -q '1 failed' fail.stderr
grep -q 'selected failure' fail.stderr

python3 - <<'PY'
from pathlib import Path
p=Path('m1/p1/tests_test.go')
p.write_text(p.read_text().replace('func TestFoo(t *testing.T) { t.Fatal("selected failure") }','func TestFoo(t *testing.T) {}'))
Path('m2/p1/broken.go').write_text('package p1\nvar broken = undefinedSymbol\n')
PY
if "$dagger_bin" check "${selection[@]}" >build.stdout 2>build.stderr; then
    echo 'Expected the selected package to fail to build' >&2; exit 1
fi
grep -q '6 passed' build.stderr
grep -q '2 failed' build.stderr
grep -q 'Go tests in m2/p1:' build.stderr
echo 'Ultrabatch checks passed'
