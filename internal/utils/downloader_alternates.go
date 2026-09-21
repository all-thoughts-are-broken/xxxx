package utils

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 本文件给下载链路加「主链 + 备用直链」的能力。
//
// 为什么是独立文件而不是给 DownloadTask 加字段：DownloadTask / Batch /
// DownloadWithRetry 是既有调用方在用的形态，加字段会波及它们。这里做成
// 加法，老路径一行不用改。

// AlternateDownloadTask 描述一次带备用直链的下载。
//
// 备用链存在的理由：图片 CDN 有多个镜像域名，接口下发的直链域名是轮换的，
// 轮换过程中会出现**已经下线**的域名（实测有的是 TLS 都握不上手，直接
// connection reset），而同一路径在其它镜像上是完好的 —— 换 host 就能下到。
// 这种情况死盯着同一条坏直链重试多少次都没用，只会白等指数退避。
type AlternateDownloadTask struct {
	Url  string
	Dist string
	// Alternates 备用直链，按顺序在主链失败后尝试。
	Alternates []string
}

// candidateURLs 返回按优先级排列的待试直链：主链在前，备用链去重后跟上。
func (t AlternateDownloadTask) candidateURLs() []string {
	out := make([]string, 0, 1+len(t.Alternates))
	seen := make(map[string]bool, 1+len(t.Alternates))
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(t.Url)
	for _, u := range t.Alternates {
		add(u)
	}
	return out
}

// DownloadWithAlternates 下载单个文件，主链失败后按顺序换备用直链。
//
// 返回实际发起的请求次数（含首次）。
func (d *Downloader) DownloadWithAlternates(
	ctx context.Context, primary string, alternates []string, dst string,
) (int, error) {
	return d.DownloadAlternateTaskWithRetry(ctx, AlternateDownloadTask{
		Url: primary, Dist: dst, Alternates: alternates,
	})
}

// DownloadAlternateTaskWithRetry 是带重试的下载主体。
//
// 重试分两层：**先在一轮内把每条候选直链都试一遍**，全都失败了才退避进入下一轮。
// 这样最常见的失败（主链域名已下线）根本不需要等退避 —— 第一轮内就靠备用链成功。
//
// 4xx（除 408/429）对**那一条**直链是确定性的，标记后不再重试，但不会因此
// 放弃其它候选：同一个路径在不同镜像上的状态未必一致。
func (d *Downloader) DownloadAlternateTaskWithRetry(
	ctx context.Context, task AlternateDownloadTask,
) (int, error) {
	urls := task.candidateURLs()
	if len(urls) == 0 {
		return 0, fmt.Errorf("下载: url 为空")
	}

	attempts := 0
	var lastErr error
	dead := make(map[string]bool, len(urls))

	for round := 0; round <= d.maxRetries; round++ {
		if round > 0 {
			select {
			case <-ctx.Done():
				return attempts, WrapCanceled(ctx, lastErr)
			case <-time.After(backoff(round)):
			}
		}

		tried := 0
		for _, u := range urls {
			if dead[u] {
				continue
			}
			tried++
			attempts++

			err := d.Download(ctx, u, task.Dist)
			if err == nil {
				return attempts, nil
			}
			lastErr = err

			if ctx.Err() != nil {
				return attempts, WrapCanceled(ctx, err)
			}
			if isDeterministicFailure(err) {
				dead[u] = true
			}
		}
		if tried == 0 {
			break // 所有候选都已被判死，再退避也没意义
		}
	}

	if len(urls) == 1 {
		return attempts, fmt.Errorf("下载失败（已尝试 %d 次）%s: %w", attempts, urls[0], lastErr)
	}
	return attempts, fmt.Errorf("下载失败（已尝试 %d 次，%d 条直链全部不可用）%s: %w",
		attempts, len(urls), urls[0], lastErr)
}

// BatchWithAlternates 是 Batch 的增强版：每个任务可带一组备用直链。
//
// 结果与入参任务按下标一一对应；onDone 在 worker 内串行调用（别做重活）。
func (d *Downloader) BatchWithAlternates(
	ctx context.Context,
	tasks []AlternateDownloadTask,
	workers int,
	onDone func(done, total int, res BatchDownloadResult),
) []BatchDownloadResult {
	results := make([]BatchDownloadResult, len(tasks))
	if len(tasks) == 0 {
		return results
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var (
		mu      sync.Mutex
		done    int
		nextIdx int
	)
	total := len(tasks)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if nextIdx >= total {
					mu.Unlock()
					return
				}
				i := nextIdx
				nextIdx++
				mu.Unlock()

				t := tasks[i]
				res := BatchDownloadResult{Url: t.Url, Dist: t.Dist}
				res.Attempts, res.Err = d.DownloadAlternateTaskWithRetry(ctx, t)
				results[i] = res

				mu.Lock()
				done++
				if onDone != nil {
					onDone(done, total, res)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return results
}
