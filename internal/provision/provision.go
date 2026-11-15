// Package provision brings accounts in.
//
// It reads a list of "email|password" lines and, for each of them,
// runs the whole chain: sign in to the account site, approve a device
// code with that session, and put the account in the database.
//
// One account at a time. The site is watching, and a hundred logins
// in a second is how a person gets noticed.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Stage is where an account's import stopped.
type Stage string

// The stages an import goes through.
const (
	// StageRead is reading the list.
	StageRead Stage = "read"
	// StageProbe is asking the site which way the account signs in.
	StageProbe Stage = "probe"
	// StagePassword is signing in with the password.
	StagePassword Stage = "password"
	// StageApprove is approving the device code.
	StageApprove Stage = "approve"
	// StageToken is turning the code into a token.
	StageToken Stage = "token"
	// StageStore is putting the account in the database.
	StageStore Stage = "store"
)

// Entry is one account from the list.
type Entry struct {
	// Email is the account's email.
	Email string
	// Password is the account's password.
	Password string
	// Line is the line it was on, for the messages.
	Line int
}

// BadLine is a line of the list that is not an account.
type BadLine struct {
	// Line is the line it was on.
	Line int
	// Text is the line itself, with the password taken off it.
	Text string
	// Reason says what is wrong with it.
	Reason string
}

// Parse reads the list.
//
// One account per line: "email|password". A line that starts with a
// "#" is a comment and an empty line is skipped. Everything after the
// first "|" is the password, so a password may contain a "|".
//
// An email that is already in the list is skipped: the second one is
// said to be a line that is not an account.
func Parse(r io.Reader) ([]Entry, []BadLine) {
	var entries []Entry
	var bad []BadLine
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, []BadLine{{Line: 0, Reason: err.Error()}}
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		n := i + 1
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		at := strings.Index(text, "|")
		if at < 0 {
			bad = append(bad, BadLine{Line: n, Text: mask(text), Reason: "没有密码"})
			continue
		}
		email := strings.TrimSpace(text[:at])
		password := text[at+1:]
		if !looksLikeEmail(email) {
			bad = append(bad, BadLine{Line: n, Text: mask(text), Reason: "不像邮箱"})
			continue
		}
		if password == "" {
			bad = append(bad, BadLine{Line: n, Text: mask(email), Reason: "没有密码"})
			continue
		}
		key := strings.ToLower(email)
		if seen[key] {
			bad = append(bad, BadLine{Line: n, Text: email, Reason: "已有这个账号"})
			continue
		}
		seen[key] = true
		entries = append(entries, Entry{Email: email, Password: password, Line: n})
	}
	return entries, bad
}

// Result is what happened to one account.
type Result struct {
	// Email is the account's email.
	Email string `json:"email"`
	// OK says whether the account is in the database now.
	OK bool `json:"ok"`
	// Stage is where it stopped.
	Stage Stage `json:"stage"`
	// Code is the site's own code, when the site gave one.
	Code int `json:"code"`
	// Message says what happened.
	Message string `json:"message"`
	// AccountID is the account's id, when it made it in.
	AccountID string `json:"account_id,omitempty"`
	// Whole says this row is not one account but the run itself.
	//
	// 整趟导入没能完成的时候，页面上要有一行说这件事。那一行不是账号，
	// 所以它不算进「失败了几个账号」里——一个账号都没处理的这一趟，失败
	// 的账号数就是零。
	Whole bool `json:"whole,omitempty"`
}

// Error is an import failure and what to do about it.
type Error struct {
	// Stage is where it happened.
	Stage Stage
	// Code is the site's own code, when the site gave one.
	Code int
	// Message says what happened.
	Message string
	// Retry says whether trying again could work.
	//
	// 这是**这一步**值不值得再试：网络抖了一下、站点说了 5xx、它在限流。
	// 链里那一步自己就补了（internal/provision/chain.go 的 step）——补一步
	// 不等于把整条链重跑一遍。
	Retry bool
	// Wait is how long to wait before trying again.
	//
	// Zero means the standard backoff: one second, then four.
	Wait time.Duration
	// Tries is how many tries at most.
	//
	// Zero means the standard number of tries. A site that is limiting
	// how much it answers is not worth three goes: the design says one.
	Tries int
	// Restart says whether the whole chain is worth running again.
	//
	// 只有一件事：设备码自己废了（过期、被拒）。那时候换一张新码才有意
	// 思 —— 别的地方都是补那一步。
	//
	// 整条链重来是「重登」那个动作的事（mmxcode relogin、调度器），不是
	// 导入链内部的兜底。
	Restart bool
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "这一步没成"
	}
	if e.Code != 0 {
		return fmt.Sprintf("%s：%s（%d）", e.Stage, msg, e.Code)
	}
	return fmt.Sprintf("%s：%s", e.Stage, msg)
}

// Importer brings one account in.
//
// The chain is in Import; the Runner drives it. Import is a field so
// that a test can bring an account in without a network.
type Importer struct {
	// Import signs the account in and puts it in the database.
	Import func(ctx context.Context, email, password string) (Result, error)
}

// Runner runs a batch.
type Runner struct {
	// Importer brings one account in.
	Importer *Importer
	// Interval is the wait between two accounts.
	Interval time.Duration
	// Sleep waits. A nil one waits with the context.
	Sleep func(ctx context.Context, d time.Duration) error
}

// backoff is how long to wait before the second and the third try.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
var backoff = []time.Duration{time.Second, 4 * time.Second}

// backoffWait gives how long to wait before the next try.
//
// The standard backoff: one second before the second try, four before
// the third. A failure that says what it wants to wait for says so, and
// this is what the others get.
func backoffWait(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(backoff) {
		return backoff[len(backoff)-1]
	}
	return backoff[attempt]
}

// Run brings in every entry and gives back what happened to each.
//
// One account does not stop the rest: a wrong password says nothing
// about the next one.
//
// The error is only for the run itself stopping -- the context was
// cancelled. An account that failed is a result, not an error.
func (r *Runner) Run(ctx context.Context, entries []Entry) ([]Result, error) {
	if r.Importer == nil || r.Importer.Import == nil {
		return nil, errors.New("provision: 没有导入器")
	}
	var out []Result
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if i > 0 && r.Interval > 0 {
			if err := r.wait(ctx, r.Interval); err != nil {
				return out, err
			}
		}
		out = append(out, r.one(ctx, e))
		// The account may have been cut short half way through, and
		// the context is checked once more here: a run that was cut
		// short must say so, even when the account it was cut short
		// on was the last one.
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// one brings one account in.
//
// 链里那一步自己扛住网络抖动（internal/provision/chain.go 的 step）：所以
// 这里不重试整条链。唯一一处整条链重来，是设备码自己废了的时候——换一
// 张新码才有可能成。
//
// 整条链重来是「重登」那个动作的事（mmxcode relogin、调度器）：账号已经
// 在池子里、令牌掉了，才轮到它。导入的时候账号还没进池子，没有「重登」
// 可言。
func (r *Runner) one(ctx context.Context, e Entry) Result {
	res, err := r.Importer.Import(ctx, e.Email, e.Password)
	if err == nil {
		if res.Email == "" {
			res.Email = e.Email
		}
		return res
	}
	var pe *Error
	if !errors.As(err, &pe) || !pe.Restart {
		return resultOf(e, err)
	}
	// 设备码废了：换一张新码，整条链再来一次。就这一次。
	res, err = r.Importer.Import(ctx, e.Email, e.Password)
	if err == nil {
		if res.Email == "" {
			res.Email = e.Email
		}
		return res
	}
	out := resultOf(e, err)
	out.Message = "换了设备码再来一次还是不成：" + out.Message
	return out
}

// resultOf turns an error into a result.
func resultOf(e Entry, err error) Result {
	res := Result{Email: e.Email}
	var pe *Error
	if errors.As(err, &pe) {
		res.Stage = pe.Stage
		res.Code = pe.Code
		res.Message = pe.Message
	} else {
		res.Stage = StageToken
		res.Message = err.Error()
	}
	return res
}

// wait waits, or gives up when the context is done.
func (r *Runner) wait(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// looksLikeEmail says whether a string could be an email.
//
// It is not a full check: the site is the one that says whether the
// account is there.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}

// mask hides the password of a line that could not be read.
//
// The text of a bad line goes into the database's import-run log and
// into the terminal, so a password must not be in it.
func mask(text string) string {
	at := strings.Index(text, "|")
	if at < 0 {
		return text
	}
	return text[:at] + "|***"
}

// ResultsJSON turns the results into the text the database keeps.
//
// 没有结果的时候给的是 `[]`，不是 `null`：页面上那个脚本
// `JSON.parse(run.results || '[]')` 解析 `null` 是成的，出来的是
// `null`，接着 `for (const r of results)` 就炸了——一整趟导入失败的
// 时候，页面上那张结果表反而打不开。
func ResultsJSON(results []Result) string {
	if len(results) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(results)
	if err != nil {
		return "[]"
	}
	return string(raw)
}
