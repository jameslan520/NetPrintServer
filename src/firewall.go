package main

import (
	"fmt"
	"strings"
)

// 防火墙规则名（删除/新增都用它，保证幂等）
const firewallRuleName = "NetPrintServer"

// firewallDeleteArgs netsh 删除规则参数
func firewallDeleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + firewallRuleName}
}

// firewallAddArgs netsh 新增规则参数。
// 注意：netsh 的参数是 enable=yes（不是 enabled=yes），写错会报「参数无效」。
func firewallAddArgs(ports []int) []string {
	var ps []string
	for _, p := range ports {
		if p > 0 {
			ps = append(ps, fmt.Sprintf("%d", p))
		}
	}
	return []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + firewallRuleName,
		"dir=in",
		"action=allow",
		"protocol=TCP",
		"localport=" + strings.Join(ps, ","),
		"profile=any",
		"enable=yes",
	}
}
