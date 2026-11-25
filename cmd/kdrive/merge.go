package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"kirius.local/kiriusdrive/client/engine"
	"math/big"
	"os"
	"strings"
	"unicode/utf8"
)

// Desktop clients use UUIDs. Encode all 128 bits in the server request-ID alphabet.
func requestULID(id string) string {
	if !requestUUID.MatchString(id) {
		return id
	}
	raw, _ := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	n := new(big.Int).SetBytes(raw)
	digit := new(big.Int)
	base := big.NewInt(32)
	var buf [26]byte
	for i := 25; i >= 0; i-- {
		n.QuoRem(n, base, digit)
		buf[i] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[digit.Int64()]
	}
	return string(buf[:])
}

func mergeCommands(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) < 2 {
		return nil, errors.New("请选择 NAS 项目")
	}
	unit := args[1]
	f := flags(args[0])
	r, e := s.Remote(ctx)
	if e != nil {
		return nil, e
	}
	switch args[0] {
	case "merge-plan":
		branch := f.String("branch", "", "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		return r.MergePlan(ctx, unit, *branch)
	case "merge-read":
		snap := f.String("snapshot", "", "")
		path := f.String("path", "", "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		return r.MergeRead(ctx, unit, *snap, *path)
	default:
		branch := f.String("branch", "", "")
		main := f.String("main", "", "")
		request := f.String("request", "", "")
		label := f.String("name", "", "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		const limit = 4 << 20
		raw, e := io.ReadAll(io.LimitReader(os.Stdin, limit+1))
		if e != nil {
			return nil, e
		}
		if len(raw) > limit || !utf8.Valid(raw) {
			return nil, errors.New("合并选择超出大小限制或并非 UTF-8")
		}
		var choices []engine.MergeChoice
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&choices); e != nil || choices == nil || dec.Decode(&struct{}{}) != io.EOF {
			return nil, errors.New("标准输入需要一份完整的 JSON 合并选择数组")
		}
		return r.MergeCommit(ctx, unit, *main, *branch, requestULID(*request), *label, choices)
	}
}
