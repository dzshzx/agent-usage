// record：把 Claude Code 交出的每周额度读数记进读数文件，供额度估算用。认得的写法与记录规则见 reading 包。
package main

import (
	"fmt"
	"io"
	"time"

	"github.com/dzshzx/agent-usage/reading"
)

// runRecord 是 agent-usage record：读 stdin、记读数。读数属于 --account 给的账号，没给时是本进程所属的
// claude-switch 名字（currentAccount）。没有要记的（没带 7 天窗口、读数没变、当前是非账号目标）照常以 0 退出；
// stdin 不是认得的写法时报错。
func runRecord(args *Args, stdin io.Reader, now time.Time) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	r, ok, err := reading.Parse(data)
	if err != nil || !ok {
		return err
	}
	name, target := args.Account, false
	if name == "" {
		name, target = currentAccount()
	}
	if target {
		return nil
	}
	if !isAccountName(name) {
		return fmt.Errorf("record: 不是账号名: '%s'", name)
	}
	_, err = reading.Append(args.DataDir, name, r, now)
	return err
}
