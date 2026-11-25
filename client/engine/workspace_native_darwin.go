//go:build darwin && cgo

package engine

/*
#cgo LDFLAGS: -lproc
#include <copyfile.h>
#include <errno.h>
#include <limits.h>
#include <libproc.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <unistd.h>

// proc_listpidspath returns byte counts, not the number of PIDs. The estimate
// can become stale between calls, and one process may have multiple matches.
static int workspace_other_holder(const char *path, int *held) {
    struct stat start, end;
    if (lstat(path, &start) != 0) return errno;
    if (!S_ISREG(start.st_mode)) return EINVAL;
    int needed;
    do {
        needed = proc_listpidspath(PROC_ALL_PIDS, 0, path,
                    PROC_LISTPIDSPATH_EXCLUDE_EVTONLY, NULL, 0);
    } while (needed < 0 && errno == EINTR);
    if (needed < 0) return errno ? errno : EIO;
    int capacity = needed > 256 ? needed : 256;
    for (;;) {
        if (capacity > INT_MAX / 2) return EOVERFLOW;
        capacity *= 2;
        pid_t *pids = malloc(capacity);
        if (pids == NULL) return ENOMEM;
        int count;
        do {
            count = proc_listpidspath(PROC_ALL_PIDS, 0, path,
                        PROC_LISTPIDSPATH_EXCLUDE_EVTONLY, pids, capacity);
        } while (count < 0 && errno == EINTR);
        if (count < 0 || count > capacity || count % sizeof(pid_t) != 0) {
            int code = count < 0 && errno ? errno : EIO;
            free(pids);
            return code;
        }
        int found = 0;
        for (int i = 0; i < count / (int)sizeof(pid_t); i++) {
            if (pids[i] != getpid()) {
                found = 1;
                break;
            }
        }
        free(pids);
        if (!found && count == capacity) continue;
        if (lstat(path, &end) != 0) return errno;
        if (start.st_ino != end.st_ino || start.st_dev != end.st_dev) return ESTALE;
        *held = found;
        return 0;
    }
}

static int workspace_copy_metadata(int from, int to) {
    return fcopyfile(from, to, NULL, COPYFILE_METADATA) == 0 ? 0 : errno;
}
*/
import "C"

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

func workspaceHeldByAnother(path string) (bool, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return false, errors.New("无效的本机文件路径")
	}
	name := C.CString(path)
	defer C.free(unsafe.Pointer(name))
	var held C.int
	if code := C.workspace_other_holder(name, &held); code != 0 {
		return false, &os.PathError{Op: "检查文件占用", Path: path, Err: syscall.Errno(code)}
	}
	return held != 0, nil
}

func workspaceCopyMetadata(src, dst *os.File) error {
	code := C.workspace_copy_metadata(C.int(src.Fd()), C.int(dst.Fd()))
	runtime.KeepAlive(src)
	runtime.KeepAlive(dst)
	if code != 0 {
		return os.NewSyscallError("fcopyfile(COPYFILE_METADATA)", syscall.Errno(code))
	}
	return nil
}
