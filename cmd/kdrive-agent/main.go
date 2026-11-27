// kdrive-agent periodically captures configured directories into immutable local
// snapshots. Network uploads require an explicit authorization.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"os"
	"os/signal"
	"time"

	"kirius.local/kiriusdrive/client/engine"
)

type watchReport struct {
	Bindings []engine.Binding  `json:"bindings"`
	Checked  map[string]string `json:"checked"`
	Changed  bool              `json:"changed"`
}

func basis(settings string, base *string) string {
	if base == nil {
		return settings
	}
	return settings + ":" + *base
}

func main() {
	once := flag.Bool("once", false, "采集所有已绑定来源一次后退出")
	interval := flag.Duration("interval", time.Minute, "重新检查来源的间隔")
	homeURL := flag.String("home-url", "", "已配对 NAS 的家庭 HTTPS IPv4 地址")
	homeIF := flag.String("home-interface", "", "登记的家庭物理网卡，例如 en0")
	small := flag.Bool("away-small", false, "外出自动上传小任务，10 MiB/批，100 MiB/24h")
	watch := flag.Bool("watch-pass", false, "一次检查，从标准输入读取持续监听且未变动的来源，输出本轮检查结果")
	flag.Parse()
	var clean map[string]string
	if *watch {
		if !*once {
			fmt.Fprintln(os.Stderr, "--watch-pass requires --once")
			os.Exit(2)
		}
		if err := json.NewDecoder(os.Stdin).Decode(&clean); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if (*homeURL == "") != (*homeIF == "") {
		fmt.Fprintln(os.Stderr, "--home-url 与 --home-interface 必须同时配置")
		os.Exit(2)
	}
	if *interval <= 0 {
		fmt.Fprintln(os.Stderr, "interval must be positive")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	s, e := engine.Open("")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	defer s.Close()
	for {
		rows, e := s.Bindings(ctx)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		report := watchReport{Bindings: rows, Checked: map[string]string{}}
		var failed bool
		policy := engine.AutoPolicy{HomeURL: *homeURL, Interface: *homeIF, AwaySmall: *small}
		for _, b := range rows {
			if ctx.Err() != nil {
				return
			}
			var settings string
			if *watch {
				settings, e = s.WatchToken(ctx, b)
				if e != nil {
					fmt.Fprintln(os.Stderr, e)
					failed = true
					continue
				}
				if clean[b.Unit] == basis(settings, b.Base) {
					continue
				}
			}
			v, scanned, e := s.CaptureScheduled(ctx, b, policy)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				failed = true
				continue
			}
			if v != nil {
				b.Base = &v.ID
				report.Changed = true
				if !*watch {
					json.NewEncoder(os.Stdout).Encode(v)
				}
			}
			if *watch && scanned {
				report.Checked[b.Unit] = basis(settings, b.Base)
			}
		}
		if *homeURL != "" || *small {
			jobs, err := s.PendingSnapshots(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				failed = true
			}
			for _, job := range jobs {
				if ctx.Err() != nil {
					return
				}
				ack, err := s.AutoSync(ctx, job, policy)
				if errors.Is(err, engine.ErrWaiting) {
					continue
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					failed = true
					continue
				}
				report.Changed = true
				if !*watch {
					json.NewEncoder(os.Stdout).Encode(ack)
				}
			}
		}
		if *once {
			if *watch {
				json.NewEncoder(os.Stdout).Encode(report)
			}
			if failed {
				os.Exit(1)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}
