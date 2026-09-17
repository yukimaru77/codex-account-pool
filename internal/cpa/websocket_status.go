package cpa

import (
	"strings"

	"github.com/tidwall/gjson"
)

// CodexWebsocketErrorStatus is the discriminator/status extraction from CPA's
// parseCodexWebsocketErrorWithCooling. No CPA payload rebuilding is included.
func CodexWebsocketErrorStatus(payload []byte) int {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return 0
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	return status
}
