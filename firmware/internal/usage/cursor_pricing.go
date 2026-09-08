package usage

import (
	"context"
	"errors"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// CursorPriceSource supplies the effective platform catalog without coupling
// the meter to catalog storage or importing its parser.
type CursorPriceSource interface {
	CursorModelPrices(context.Context) map[string]Pricing
}

// SetCursorPriceSource is wired once before serving requests.
func (m *Meter) SetCursorPriceSource(source CursorPriceSource) { m.cursorPrices = source }

// ParseCursorPrice validates a platform catalog price in integer micro-yuan per
// million tokens. Missing cache prices use the input price; explicit zero is free.
func ParseCursorPrice(raw string) (Pricing, error) {
	price, err := ParsePricing(raw)
	if err != nil {
		return nil, errors.New("Cursor 模型价格字段或金额无效")
	}
	if _, ok := price[FieldIn]; !ok {
		return nil, errors.New("Cursor 模型价格必须包含输入价和输出价")
	}
	if _, ok := price[FieldOut]; !ok {
		return nil, errors.New("Cursor 模型价格必须包含输入价和输出价")
	}
	for field, value := range price {
		if (field != FieldIn && field != FieldOut && field != FieldCacheRead && field != FieldCacheWrite) || value < 0 || value > store.MaxPricingMicro {
			return nil, errors.New("Cursor 模型价格字段或金额无效")
		}
	}
	return price, nil
}
