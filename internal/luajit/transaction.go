package luajit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type transactionEntry struct {
	Path    string
	Backup  string
	Existed bool
}
type transaction struct {
	root    string
	entries []transactionEntry
}

func beginTransaction(root string, paths []string) (*transaction, error) {
	dir := filepath.Join(root, ".dst-admin-luajit-transaction")
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, fmt.Errorf("存在未完成的 LuaJIT 安装或无法创建备份目录: %w", err)
	}
	tx := &transaction{root: dir}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	for i, p := range paths {
		e := transactionEntry{Path: p, Backup: filepath.Join(dir, strconv.Itoa(i))}
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("LuaJIT 安装目标不能是符号链接")
			}
			e.Existed = true
			if err = copyPath(p, e.Backup); err != nil {
				return nil, err
			}
		}
		tx.entries = append(tx.entries, e)
	}
	data, _ := json.Marshal(tx.entries)
	if err := writeFile(filepath.Join(dir, "journal.json"), data, 0600); err != nil {
		return nil, err
	}
	ok = true
	return tx, nil
}
func (tx *transaction) rollback() error {
	var errs []error
	for i := len(tx.entries) - 1; i >= 0; i-- {
		e := tx.entries[i]
		if err := os.RemoveAll(e.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		if e.Existed {
			if err := copyPath(e.Backup, e.Path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return os.RemoveAll(tx.root)
}
func (tx *transaction) complete() error {
	if err := writeFile(filepath.Join(tx.root, "committed"), []byte("1\n"), 0600); err != nil {
		return err
	}
	return os.RemoveAll(tx.root)
}

func safeParent(root, target string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("LuaJIT 安装路径超出 DST 目录")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("LuaJIT 安装路径不能经过符号链接: %s", current)
		}
	}
	return nil
}

func recoverTransaction(root string, paths []string) error {
	dir := filepath.Join(root, ".dst-admin-luajit-transaction")
	if err := safeParent(root, dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "committed")); err == nil {
		return os.RemoveAll(dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, "journal.json"))
	// No journal means backup preparation was interrupted before any mutation.
	if os.IsNotExist(err) {
		return os.RemoveAll(dir)
	}
	if err != nil {
		return err
	}
	tx := &transaction{root: dir}
	if err = json.Unmarshal(data, &tx.entries); err != nil {
		return err
	}
	if len(tx.entries) != len(paths) {
		return errors.New("LuaJIT 恢复日志无效，备份已保留")
	}
	for i, e := range tx.entries {
		if e.Path != paths[i] || e.Backup != filepath.Join(dir, strconv.Itoa(i)) {
			return errors.New("LuaJIT 恢复路径与当前安装不一致")
		}
		if err := safeParent(root, e.Path); err != nil {
			return err
		}
		if e.Existed {
			if _, err := os.Lstat(e.Backup); err != nil {
				return fmt.Errorf("LuaJIT 恢复备份缺失: %w", err)
			}
		}
	}
	return tx.rollback()
}
func copyPath(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			value, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			return os.Symlink(value, target)
		}
		if !info.Mode().IsRegular() {
			return errors.New("备份包含不支持的文件类型")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		closeErr := w.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
