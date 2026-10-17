#!/usr/bin/env bash
# Run the blockvol Btrfs integration suite on a Linux test host.
#
# Build on the workstation, from the runtime repo root:
#   GOOS=linux GOARCH=amd64 go test -c -o /tmp/blockvol.test ./packages/orchestrator/pkg/blockvol/
#   scp /tmp/blockvol.test packages/orchestrator/pkg/blockvol/testdata/run-on-host.sh <host>:/tmp/
# Then on the host (needs passwordless sudo, btrfs-progs, e2fsprogs, util-linux):
#   bash /tmp/run-on-host.sh /tmp/blockvol.test
#
# Everything lives under WORK (default /var/lib/e2b-dev/blockvol): a sparse
# image -> loop device -> Btrfs -> mount. The trap unmounts, detaches the loop
# device and removes the image whatever the outcome. Nothing else on the host
# is touched.
set -euo pipefail

TEST_BIN=${1:?usage: run-on-host.sh /path/to/blockvol.test [extra go test flags]}
shift || true
WORK=${BLOCKVOL_WORK:-/var/lib/e2b-dev/blockvol}
IMG="$WORK/btrfs.img"
MNT="$WORK/mnt"
SIZE=${BLOCKVOL_IMAGE_SIZE:-20G}
LOOP=""

case "$WORK" in
  /var/lib/e2b-dev/*) ;;
  *) echo "refusing WORK=$WORK outside /var/lib/e2b-dev/" >&2; exit 2 ;;
esac

for tool in btrfs mkfs.btrfs mkfs.ext4 tune2fs e2fsck blkid fallocate losetup; do
  sudo sh -c "command -v $tool" >/dev/null || { echo "missing $tool" >&2; exit 2; }
done

cleanup() {
  set +e
  if mountpoint -q "$MNT"; then sudo umount "$MNT"; fi
  if [ -n "$LOOP" ]; then sudo losetup -d "$LOOP"; fi
  sudo rm -f "$IMG"
  sudo rmdir "$MNT" 2>/dev/null
  sudo rmdir "$WORK" 2>/dev/null
}
trap cleanup EXIT

if [ -e "$IMG" ] || mountpoint -q "$MNT" 2>/dev/null; then
  echo "$IMG or $MNT already in use; clean up the previous run first" >&2
  trap - EXIT
  exit 2
fi

sudo mkdir -p "$MNT"
sudo truncate -s "$SIZE" "$IMG"
LOOP=$(sudo losetup --find --show "$IMG")
sudo mkfs.btrfs -q -f "$LOOP"
sudo mount "$LOOP" "$MNT"

echo "== host: $(uname -r); $(btrfs --version); $(sudo mke2fs -V 2>&1 | head -1)"
echo "== btrfs on $LOOP mounted at $MNT"

set +e
# Unit tests read fixtures relative to the package directory and already run
# on the workstation; by default run only the Btrfs-backed suite. Pass your
# own -test.run to override.
if [ "$#" -eq 0 ]; then set -- -test.run '^TestIntegration'; fi
sudo BLOCKVOL_TEST_ROOT="$MNT" "$TEST_BIN" -test.v -test.count=1 "$@"
rc=$?
set -e

echo "== leftovers under $MNT after the suite (expect none):"
sudo find "$MNT" -mindepth 1 -maxdepth 2 | head -20
exit $rc
