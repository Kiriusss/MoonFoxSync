// kdrive-agent periodically captures configured directories into immutable local
// snapshots. Network uploads require an explicit authorization.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"kirius.local/kiriusdrive/client/engine"
	"os"
	"os/signal"
	"time"
)

func main() {
	once := flag.Bool("once", false, "采集所有已绑定来源一次后退出")
	interval := flag.Duration("interval", time.Minute, "重新检查来源的间隔")
	homeURL := flag.String("home-url", "", "已配对 NAS 的家庭 HTTPS IPv4 地址")
	homeIF := flag.String("home-interface", "", "登记的家庭物理网卡，例如 en0")
	small := flag.Bool("away-small", false, "外出自动上传小任务，10 MiB/批，100 MiB/24h")
	flag.Parse()
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
		var failed bool
		policy := engine.AutoPolicy{HomeURL: *homeURL, Interface: *homeIF, AwaySmall: *small}
		for _, b := range rows {
			v, e := s.CaptureDue(ctx, b, policy)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				failed = true
				continue
			}
			if v != nil {
				json.NewEncoder(os.Stdout).Encode(v)
			}
		}
		if *homeURL != "" || *small {
			jobs, err := s.Queue(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				failed = true
			}
			for _, job := range jobs {
				if job.State != "queued" {
					continue
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
				json.NewEncoder(os.Stdout).Encode(ack)
			}
		}
		if *once {
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
