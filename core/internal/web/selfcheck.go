package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/relocode/relo/internal/web/cdp"
)

// stealthProbe only reads browser identity. It does not patch page properties
// or add noise to any fingerprint.
const stealthProbe = `(async () => {
  const o = {webdriver: navigator.webdriver, ua: navigator.userAgent};
  const d = navigator.userAgentData;
  if (d) Object.assign(o, await d.getHighEntropyValues(['uaFullVersion','platform']));
  return JSON.stringify(o);
})()`

type stealthState struct {
	Webdriver     bool   `json:"webdriver"`
	UA            string `json:"ua"`
	UAFullVersion string `json:"uaFullVersion"`
	Platform      string `json:"platform"`
}

func logStealthSelfCheck(ctx context.Context, session *cdp.Session, tier Tier, hooks Hooks) {
	if hooks.Log == nil {
		return
	}
	raw, err := session.Evaluate(ctx, stealthProbe)
	if err != nil {
		hooks.Log(fmt.Sprintf("web stealth self-check lane=%s error=%q", tier, err.Error()))
		return
	}
	var state stealthState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		hooks.Log(fmt.Sprintf("web stealth self-check lane=%s decode error=%q", tier, err.Error()))
		return
	}
	hooks.Log(fmt.Sprintf("web stealth self-check lane=%s webdriver=%t headlessUA=%t uaFullVersion=%t platform=%s",
		tier, state.Webdriver, strings.Contains(state.UA, "HeadlessChrome"), state.UAFullVersion != "", state.Platform))
}
