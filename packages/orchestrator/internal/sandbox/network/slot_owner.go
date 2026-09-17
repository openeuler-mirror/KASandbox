package network

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 槽位归属标记：编排器 Acquire 到本地槽位 ns-N 时，在 slotOwnerDir/ns-N 记录
// 自己的进程身份（pid + 进程启动时间，后者用于抵御 pid 复用）。
//
// 编排器被 SIGKILL（例如 Pool.Close 串行清理超出 kill_timeout）后，池里的
// netns 仍是有效挂载、veth 与 iptables 规则也都在，仅凭 /run/netns 无法区分
// 「别的程序在用」和「上一代编排器遗留」。有了归属标记，下一代启动时就能把
// owner 已不存在的槽位安全回收，而不是永久标记为 foreign 直到槽位耗尽。
const slotOwnerDir = "/run/e2b-netslots"

type processIdentity struct {
	pid       int
	startTime uint64 // /proc/<pid>/stat 第 22 字段，单位 clock tick
}

func (id processIdentity) String() string {
	return fmt.Sprintf("%d %d", id.pid, id.startTime)
}

// alive 判断该身份对应的进程是否仍存活（pid 存在且启动时间一致）。
func (id processIdentity) alive() bool {
	current, err := processIdentityOf(id.pid)
	if err != nil {
		return false
	}

	return current == id
}

func currentProcessIdentity() (processIdentity, error) {
	return processIdentityOf(os.Getpid())
}

func processIdentityOf(pid int) (processIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processIdentity{}, err
	}

	// comm 字段可能含空格和括号，从最后一个 ')' 之后再切分
	rest := string(data)
	idx := strings.LastIndexByte(rest, ')')
	if idx < 0 {
		return processIdentity{}, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(rest[idx+1:])
	// ')' 之后依次为 state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime，starttime 是第 20 个（下标 19）
	if len(fields) < 20 {
		return processIdentity{}, fmt.Errorf("malformed /proc/%d/stat: %d fields after comm", pid, len(fields))
	}

	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processIdentity{}, fmt.Errorf("parse starttime in /proc/%d/stat: %w", pid, err)
	}

	return processIdentity{pid: pid, startTime: startTime}, nil
}

func slotOwnerPath(slotName string) string {
	return filepath.Join(slotOwnerDir, slotName)
}

func writeSlotOwner(slotName string, id processIdentity) error {
	if err := os.MkdirAll(slotOwnerDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", slotOwnerDir, err)
	}

	// 先写临时文件再 rename，避免异常退出留下半截内容
	tmp := slotOwnerPath(slotName) + ".tmp"
	if err := os.WriteFile(tmp, []byte(id.String()), 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, slotOwnerPath(slotName))
}

func removeSlotOwner(slotName string) error {
	err := os.Remove(slotOwnerPath(slotName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// readSlotOwner 读取槽位归属标记；标记不存在时 found 为 false。
func readSlotOwner(slotName string) (id processIdentity, found bool, err error) {
	data, err := os.ReadFile(slotOwnerPath(slotName))
	if errors.Is(err, os.ErrNotExist) {
		return processIdentity{}, false, nil
	}
	if err != nil {
		return processIdentity{}, false, err
	}

	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return processIdentity{}, false, fmt.Errorf("malformed slot owner marker %s: %q", slotName, string(data))
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return processIdentity{}, false, fmt.Errorf("malformed slot owner marker %s: %w", slotName, err)
	}
	startTime, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return processIdentity{}, false, fmt.Errorf("malformed slot owner marker %s: %w", slotName, err)
	}

	return processIdentity{pid: pid, startTime: startTime}, true, nil
}
