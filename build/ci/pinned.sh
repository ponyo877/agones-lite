#!/bin/sh
# Checks that the Agones in third_party is the version the build uses.
#
# third_party/agones is a reference copy: the comments name files in it,
# and some tests read its samples. The code that is built comes from the
# Go module. The two must be the same commit, which is the one upstream
# has under the tag of the module's version. And the copy must be as
# upstream has it: nobody edits it.
set -eu
version=$(go list -m -f '{{.Version}}' agones.dev/agones)
url=$(git config -f .gitmodules submodule.third_party/agones.url)
have=$(git rev-parse HEAD:third_party/agones)
# An annotated tag is listed twice, the commit it stands for last.
want=$(git ls-remote "$url" "refs/tags/$version" "refs/tags/$version^{}" | tail -1 | cut -f1)

echo "go.mod has agones.dev/agones $version, which is commit $want"
echo "third_party/agones is at commit $have"
[ "$have" = "$want" ]
if [ -e third_party/agones/.git ]; then
	changed=$(git -C third_party/agones status --porcelain)
	[ -z "$changed" ] || { echo "third_party/agones has been changed:"; echo "$changed"; exit 1; }
fi
