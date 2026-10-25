#!/usr/bin/env bash
set -euo pipefail

module_source=$(cd "$(dirname "$0")/.." && pwd)
probe=$(mktemp -d)
dagger_bin=${DAGGER_BIN:-dagger}
module_name=${MODULE_NAME:-go}
trap 'result=$?; if [ "$result" -eq 0 ] && [ "${KEEP_ULTRABATCH_FIXTURE:-0}" != 1 ]; then rm -rf "$probe"; else printf "Fixture and logs: %s\n" "$probe"; fi' EXIT

python3 - "$probe" "$module_source" "$module_name" <<'PY'
import json, sys
from pathlib import Path
root=Path(sys.argv[1])
(root/'dagger.toml').write_text('[modules.'+sys.argv[3]+']\nsource = '+json.dumps(sys.argv[2])+'\n')
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
check=("$dagger_bin" check --module "$module_name" --check test)
selection=("--$module_name-package=m1/p1" "--$module_name-package=m1/p2" "--$module_name-package=m2/p1" "--$module_name-package=m2/p2" "--$module_name-test=TestFoo" "--$module_name-test=TestBar")
"${check[@]}" "${selection[@]}" >pass.stdout 2>pass.stderr
grep -q '8 passed' pass.stderr

# A cached module index must not retain an earlier package or test selection.
if "${check[@]}" "--$module_name-package=m1/p1" "--$module_name-test=TestOther" >other.stdout 2>other.stderr; then
    echo 'Expected the newly selected test to fail' >&2; exit 1
fi
grep -q '1 failed' other.stderr
grep -q 'excluded test ran' other.stderr
"${check[@]}" "--$module_name-package=m1/unused" "--$module_name-test=TestFoo" >changed-selection.stdout 2>changed-selection.stderr
grep -q '1 passed' changed-selection.stderr
"${check[@]}" "${selection[@]}" >repeat.stdout 2>repeat.stderr
grep -q '8 passed' repeat.stderr

# A source change must invalidate the cached index, including new names.
printf '\nfunc TestAdded(t *testing.T) {}\n' >>m1/p1/tests_test.go
"${check[@]}" "--$module_name-package=m1/p1" "--$module_name-test=TestAdded" >added.stdout 2>added.stderr
grep -q '1 passed' added.stderr

python3 - <<'PY'
from pathlib import Path
p=Path('m1/p1/tests_test.go')
p.write_text(p.read_text().replace('func TestFoo(t *testing.T) {}','func TestFoo(t *testing.T) { t.Fatal("selected failure") }'))
PY
if "${check[@]}" "${selection[@]}" >fail.stdout 2>fail.stderr; then
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
if "${check[@]}" "${selection[@]}" >build.stdout 2>build.stderr; then
    echo 'Expected the selected package to fail to build' >&2; exit 1
fi
grep -q '6 passed' build.stderr
grep -q '2 failed' build.stderr
grep -q 'Go tests in m2/p1:' build.stderr
# Runtime skips and build constraints must retain their outcomes.
rm m2/p1/broken.go
cat >m1/p1/skip_test.go <<'GO'
package p1
import "testing"
func TestSkipped(t *testing.T) { t.Skip("intentional skip") }
GO
cat >m1/p1/tagged_test.go <<'GO'
//go:build ultrabatch_excluded

package p1
import "testing"
func TestTaggedOut(t *testing.T) { t.Fatal("build constraint ignored") }
GO
"${check[@]}" "--$module_name-package=m1/p1" "--$module_name-test=TestSkipped" "--$module_name-test=TestTaggedOut" >skip.stdout 2>skip.stderr
grep -q '2 passed' skip.stderr
echo 'Ultrabatch checks passed'
