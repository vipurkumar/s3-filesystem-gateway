#!/usr/bin/env bash
set -euo pipefail

# Configuration
GATEWAY_HOST="${GATEWAY_HOST:-localhost}"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-localhost:9000}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-minioadmin}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-minioadmin}"
MINIO_BUCKET="${MINIO_BUCKET:-data}"
MOUNT_POINT="/tmp/s3gw-test-$$"

# Test counters
PASSED=0
FAILED=0
TESTS_RUN=0

# Colors for output
pass() { echo -e "\033[32m✓ $1\033[0m"; PASSED=$((PASSED + 1)); }
fail() { echo -e "\033[31m✗ $1\033[0m"; FAILED=$((FAILED + 1)); }
info() { echo -e "\033[34m→ $1\033[0m"; }

# S3 helper using curl (works without mc installed)
s3_put() {
    local key="$1"
    local body="$2"
    local date
    date=$(date -R)
    curl -s -X PUT \
        "http://${MINIO_ENDPOINT}/${MINIO_BUCKET}/${key}" \
        -H "Host: ${MINIO_ENDPOINT}" \
        -H "Date: ${date}" \
        -u "${MINIO_ACCESS_KEY}:${MINIO_SECRET_KEY}" \
        -d "${body}" \
        -o /dev/null -w "%{http_code}"
}

s3_get() {
    local key="$1"
    curl -s \
        "http://${MINIO_ENDPOINT}/${MINIO_BUCKET}/${key}" \
        -u "${MINIO_ACCESS_KEY}:${MINIO_SECRET_KEY}"
}

s3_head() {
    local key="$1"
    curl -s -o /dev/null -w "%{http_code}" -I \
        "http://${MINIO_ENDPOINT}/${MINIO_BUCKET}/${key}" \
        -u "${MINIO_ACCESS_KEY}:${MINIO_SECRET_KEY}"
}

s3_delete() {
    local key="$1"
    curl -s -X DELETE \
        "http://${MINIO_ENDPOINT}/${MINIO_BUCKET}/${key}" \
        -u "${MINIO_ACCESS_KEY}:${MINIO_SECRET_KEY}" \
        -o /dev/null -w "%{http_code}"
}

# Cleanup function — always runs on exit
cleanup() {
    info "Cleaning up..."

    # Unmount if mounted
    if mountpoint -q "${MOUNT_POINT}" 2>/dev/null; then
        umount -f "${MOUNT_POINT}" 2>/dev/null || umount -l "${MOUNT_POINT}" 2>/dev/null || true
    fi

    # Remove mount point directory
    rmdir "${MOUNT_POINT}" 2>/dev/null || true

    # Remove test data from S3
    s3_delete "test-upload.txt" >/dev/null 2>&1 || true
    s3_delete "test-write.txt" >/dev/null 2>&1 || true
    s3_delete "testdir/nested.txt" >/dev/null 2>&1 || true
    s3_delete "testdir/" >/dev/null 2>&1 || true

    info "Cleanup complete."
}

trap cleanup EXIT

run_test() {
    local name="$1"
    shift
    TESTS_RUN=$((TESTS_RUN + 1))
    if "$@"; then
        pass "${name}"
    else
        fail "${name}"
    fi
}

# ---------------------------------------------------------------------------
# Test implementations
# ---------------------------------------------------------------------------

test_upload_to_s3() {
    local status
    status=$(s3_put "test-upload.txt" "s3-test-content-12345")
    [[ "${status}" == "200" ]]
}

test_mount_nfs() {
    mkdir -p "${MOUNT_POINT}"
    if [[ -n "${MOUNT_OPTS:-}" ]]; then
        mount -t nfs4 -o "${MOUNT_OPTS}" "${GATEWAY_HOST}:/" "${MOUNT_POINT}"
    else
        mount -t nfs4 "${GATEWAY_HOST}:/" "${MOUNT_POINT}"
    fi
    mountpoint -q "${MOUNT_POINT}"
}

test_ls() {
    # Give the gateway a moment to reflect the S3 state
    sleep 1
    ls "${MOUNT_POINT}" | grep -q "test-upload.txt"
}

test_cat() {
    local content
    content=$(cat "${MOUNT_POINT}/test-upload.txt")
    [[ "${content}" == "s3-test-content-12345" ]]
}

test_stat() {
    local size
    size=$(stat -c "%s" "${MOUNT_POINT}/test-upload.txt" 2>/dev/null || stat -f "%z" "${MOUNT_POINT}/test-upload.txt" 2>/dev/null)
    # "s3-test-content-12345" is 21 bytes
    [[ "${size}" -eq 21 ]]
}

test_write() {
    echo "hello" > "${MOUNT_POINT}/test-write.txt"
    [[ -f "${MOUNT_POINT}/test-write.txt" ]]
}

test_write_reached_s3() {
    # Allow time for the write to propagate to S3
    sleep 2
    local content
    content=$(s3_get "test-write.txt")
    # echo adds a trailing newline, so expect "hello\n"
    [[ "${content}" == "hello" ]] || [[ "${content}" == $'hello\n' ]]
}

test_mkdir() {
    mkdir "${MOUNT_POINT}/testdir"
    [[ -d "${MOUNT_POINT}/testdir" ]]
}

test_write_in_subdir() {
    echo "nested" > "${MOUNT_POINT}/testdir/nested.txt"
    local content
    content=$(cat "${MOUNT_POINT}/testdir/nested.txt")
    [[ "${content}" == "nested" ]]
}

test_delete() {
    rm "${MOUNT_POINT}/test-write.txt"
    [[ ! -f "${MOUNT_POINT}/test-write.txt" ]]
}

test_delete_in_s3() {
    # Allow time for the delete to propagate to S3
    sleep 2
    local status
    status=$(s3_head "test-write.txt")
    # Expect 404 or 403 (object not found)
    [[ "${status}" == "404" ]] || [[ "${status}" == "403" ]]
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "Starting NFS integration tests"
info "Gateway: ${GATEWAY_HOST} | MinIO: ${MINIO_ENDPOINT} | Bucket: ${MINIO_BUCKET}"
echo ""

run_test "Upload test data to S3 via MinIO" test_upload_to_s3
run_test "Mount NFS share" test_mount_nfs
run_test "List directory shows uploaded file" test_ls
run_test "Read file content matches upload" test_cat
run_test "Stat reports correct file size" test_stat
run_test "Write file through NFS" test_write
run_test "Write propagated to S3" test_write_reached_s3
run_test "Create directory through NFS" test_mkdir
run_test "Write file in subdirectory" test_write_in_subdir
run_test "Delete file through NFS" test_delete
run_test "Delete propagated to S3" test_delete_in_s3

echo ""
echo "========================================"
echo "  Results: ${PASSED} passed, ${FAILED} failed (${TESTS_RUN} total)"
echo "========================================"

if [[ "${FAILED}" -gt 0 ]]; then
    exit 1
fi

exit 0
