// Model prices and the arithmetic that turns token counts into money.
//
// Prices are entered by an administrator in yuan per million tokens, one price
// per kind of token. Money is computed and stored as an integer number of
// nano-yuan (1e-9 yuan): adding up floating-point amounts across thousands of
// calls drifts, and a ledger has to add up.
//
// Which model a call is priced as is not always the model that was requested:
// routers pick a model per call (openrouter/auto) and servers answer under
// aliases (a request for deepseek-chat comes back as deepseek-flash). So the
// model that actually answered is looked up first, and the requested one second.
package main

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// maxPricePerMillion bounds an entered price. It is far above any real price
// and exists to catch a typo (an extra few zeros) before it prices every call.
const maxPricePerMillion = 100000

// modelPrice is one row of the price table, in yuan per million tokens. The
// cache prices are optional: an unset one falls back to the input price, which
// is what a provider without a cache discount charges.
type modelPrice struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Input      float64   `json:"input"`
	CacheRead  *float64  `json:"cacheRead,omitempty"`
	CacheWrite *float64  `json:"cacheWrite,omitempty"`
	Output     float64   `json:"output"`
	UpdatedBy  string    `json:"updatedBy,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
}

// priceSnapshot is the price actually applied to a call, with the fallbacks
// already resolved. It is copied into the ledger entry, so a later change to the
// price table never changes what an old call cost.
type priceSnapshot struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Output     float64 `json:"output"`
}

// effective resolves the optional cache prices.
func (p modelPrice) effective() priceSnapshot {
	snap := priceSnapshot{Input: p.Input, CacheRead: p.Input, CacheWrite: p.Input, Output: p.Output}
	if p.CacheRead != nil {
		snap.CacheRead = *p.CacheRead
	}
	if p.CacheWrite != nil {
		snap.CacheWrite = *p.CacheWrite
	}
	return snap
}

// validate normalizes a submitted row.
func (p modelPrice) validate() (modelPrice, error) {
	p.Provider = strings.ToLower(strings.TrimSpace(p.Provider))
	p.Model = strings.TrimSpace(p.Model)
	if p.Provider == "" || p.Model == "" {
		return p, errors.New("provider 和模型都不能为空")
	}
	for _, v := range []*float64{&p.Input, &p.Output, p.CacheRead, p.CacheWrite} {
		if v == nil {
			continue
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > maxPricePerMillion {
			return p, errors.New("单价须在 0 到 100000 元/百万 token 之间")
		}
	}
	return p, nil
}

// priceKey identifies a row. Model ids are matched exactly: they are opaque
// strings, and "gpt-4o" and "gpt-4o-mini" must never collide.
func priceKey(providerName, model string) string {
	return strings.ToLower(strings.TrimSpace(providerName)) + "\x00" + strings.TrimSpace(model)
}

// nanoPerMillion converts a price in yuan per million tokens to nano-yuan per
// million tokens, rounding to the nearest integer.
func nanoPerMillion(yuanPerMillion float64) *big.Int {
	return big.NewInt(int64(math.Round(yuanPerMillion * 1e9)))
}

// costOf prices one kind of token: tokens × price / 1e6, rounded half up. The
// multiplication is done in big.Int because tokens × nano-price can exceed the
// int64 range for a long enough call at a high enough price.
func costOf(tokens int, yuanPerMillion float64) int64 {
	if tokens <= 0 || yuanPerMillion <= 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(int64(tokens)), nanoPerMillion(yuanPerMillion))
	product.Add(product, big.NewInt(500000))
	product.Quo(product, big.NewInt(1000000))
	return product.Int64()
}

// rate prices a call's usage. The four counts never overlap (see
// agentcore.Usage), so they are priced independently and summed; reasoning is
// already inside output and is not charged again.
func rate(u agentcore.Usage, p priceSnapshot) int64 {
	return costOf(u.InputTokens, p.Input) +
		costOf(u.CacheReadTokens, p.CacheRead) +
		costOf(u.CacheWriteTokens, p.CacheWrite) +
		costOf(u.OutputTokens, p.Output)
}

// yuan converts a stored amount to yuan for display.
func yuan(nano int64) float64 {
	return float64(nano) / 1e9
}

// --- the price table, kept in settings.json ---------------------------------

// prices returns the table.
func (s *settingsStore) prices() []modelPrice {
	return s.get().ModelPrices
}

// priceFor finds the price to apply to a call: the model that answered first,
// then the one requested. ok is false when neither is priced, in which case the
// call is recorded at zero and flagged so the gap can be found.
func (s *settingsStore) priceFor(providerName, responseModel, model string) (priceSnapshot, bool) {
	rows := map[string]modelPrice{}
	for _, row := range s.prices() {
		rows[priceKey(row.Provider, row.Model)] = row
	}
	for _, candidate := range []string{responseModel, model} {
		if candidate == "" {
			continue
		}
		if row, ok := rows[priceKey(providerName, candidate)]; ok {
			return row.effective(), true
		}
	}
	return priceSnapshot{}, false
}

// putPrice adds or replaces a row.
func (s *settingsStore) putPrice(next modelPrice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]modelPrice(nil), s.settings.ModelPrices...)
	key := priceKey(next.Provider, next.Model)
	replaced := false
	for i, row := range s.settings.ModelPrices {
		if priceKey(row.Provider, row.Model) == key {
			s.settings.ModelPrices[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		s.settings.ModelPrices = append(s.settings.ModelPrices, next)
	}
	if err := s.saveLocked(); err != nil {
		s.settings.ModelPrices = previous
		return err
	}
	return nil
}

// removePrice drops a row, reporting whether it existed.
func (s *settingsStore) removePrice(providerName, model string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := priceKey(providerName, model)
	kept := make([]modelPrice, 0, len(s.settings.ModelPrices))
	for _, row := range s.settings.ModelPrices {
		if priceKey(row.Provider, row.Model) != key {
			kept = append(kept, row)
		}
	}
	if len(kept) == len(s.settings.ModelPrices) {
		return false, nil
	}
	previous := s.settings.ModelPrices
	s.settings.ModelPrices = kept
	if err := s.saveLocked(); err != nil {
		s.settings.ModelPrices = previous
		return false, err
	}
	return true, nil
}
