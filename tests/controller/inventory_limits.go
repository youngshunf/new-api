package controllertests

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// VerifyInventorySupplyLimits 通过生产构建接缝验证完整库存不会被截断或无限编码。
func VerifyInventorySupplyLimits(t *testing.T, build func([]model.Pricing, []model.PricingVendor, map[string]common.EndpointInfo, map[string]float64, time.Time) (*dto.LlmModelInventory, error)) {
	pricing := make([]model.Pricing, 4096)
	for index := range pricing {
		pricing[index] = model.Pricing{ModelName: fmt.Sprintf("model-%04d", index), EnableGroup: []string{"default"}}
	}
	t.Run("模型数边界保持完整", func(t *testing.T) {
		inventory, err := build(pricing, nil, nil, nil, time.Unix(1700000000, 0))
		require.NoError(t, err)
		assert.Len(t, inventory.Models, 4096)
		assert.Equal(t, "model-4095", inventory.Models[4095].ModelName)
	})
	t.Run("第4097项整包拒绝", func(t *testing.T) {
		oversized := append(append([]model.Pricing(nil), pricing...), model.Pricing{ModelName: "overflow"})
		inventory, err := build(oversized, nil, nil, nil, time.Unix(1700000000, 0))
		require.ErrorIs(t, err, dto.ErrLlmInventoryLimits, "不能截成前4096项交付成功快照")
		assert.Nil(t, inventory)
	})
	t.Run("模型编码大小而非原字符串长度", func(t *testing.T) {
		entry := dto.LlmModelInventoryEntry{ModelName: "model", EnableGroups: []string{"default"}, SupportedEndpointTypes: []string{"openai"}}
		// description 非空时增加字段本身；ASCII说明文案可精确填到4096字节。
		entry.Description = "x"
		withDescription, err := common.Marshal(entry)
		require.NoError(t, err)
		atLimit := 4096 - len(withDescription) + 1
		for _, extra := range []int{0, 1} {
			input := []model.Pricing{{ModelName: entry.ModelName, EnableGroup: entry.EnableGroups, SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAI}, Description: strings.Repeat("x", atLimit+extra)}}
			inventory, buildErr := build(input, nil, nil, nil, time.Unix(1700000000, 0))
			if extra == 0 {
				require.NoError(t, buildErr)
				encoded, encodeErr := common.Marshal(inventory.Models[0])
				require.NoError(t, encodeErr)
				assert.Len(t, encoded, 4096)
			} else {
				require.ErrorIs(t, buildErr, dto.ErrLlmInventoryLimits)
				assert.Nil(t, inventory)
			}
		}
	})
	t.Run("转义后越界也拒绝", func(t *testing.T) {
		inventory, err := build([]model.Pricing{{ModelName: "escaped", Description: strings.Repeat("<", 700)}}, nil, nil, nil, time.Unix(1700000000, 0))
		require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
		assert.Nil(t, inventory)
	})
	t.Run("集合合法等边界", func(t *testing.T) {
		input := []model.Pricing{{ModelName: "bounded"}}
		for i := 0; i < 128; i++ {
			input[0].EnableGroup = append(input[0].EnableGroup, fmt.Sprintf("g%03d", i))
		}
		for i := 0; i < 32; i++ {
			input[0].SupportedEndpointTypes = append(input[0].SupportedEndpointTypes, constant.EndpointType(fmt.Sprintf("e%02d", i)))
		}
		vendors := make([]model.PricingVendor, 256)
		for i := range vendors {
			vendors[i] = model.PricingVendor{ID: 256 - i, Name: fmt.Sprint(i)}
		}
		endpoints := map[string]common.EndpointInfo{}
		for i := 0; i < 32; i++ {
			endpoints[fmt.Sprint(i)] = common.EndpointInfo{Path: "/v1"}
		}
		ratios := map[string]float64{}
		for i := 0; i < 128; i++ {
			ratios[fmt.Sprint(i)] = 1
		}
		value, err := build(input, vendors, endpoints, ratios, time.Unix(1700000000, 0))
		require.NoError(t, err)
		assert.Len(t, value.Models[0].EnableGroups, 128)
		assert.Len(t, value.Models[0].SupportedEndpointTypes, 32)
		assert.Len(t, value.Vendors, 256)
		assert.Equal(t, 1, value.Vendors[0].VendorId)
		assert.Equal(t, 256, value.Vendors[255].VendorId)
		assert.Len(t, value.GroupRatio, 128)
		assert.Len(t, value.SupportedEndpoint, 32)
	})
	t.Run("去重摘要等价且共享缓存不变", func(t *testing.T) {
		input := []model.Pricing{{ModelName: "bounded", EnableGroup: []string{"vip", "default", "vip"},
			SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAIResponse, constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}}}
		value, err := build(input, nil, nil, nil, time.Unix(1700000000, 0))
		require.NoError(t, err)
		assert.Equal(t, []string{"default", "vip"}, value.Models[0].EnableGroups)
		assert.Equal(t, []string{string(constant.EndpointTypeOpenAI), string(constant.EndpointTypeOpenAIResponse)}, value.Models[0].SupportedEndpointTypes)
		assert.Equal(t, []string{"vip", "default", "vip"}, input[0].EnableGroup)
		assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAIResponse, constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}, input[0].SupportedEndpointTypes)
		canonical := []model.Pricing{{ModelName: "bounded", EnableGroup: []string{"default", "vip"},
			SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}}}
		equivalent, err := build(canonical, nil, nil, nil, time.Unix(1700000000, 0))
		require.NoError(t, err)
		assert.Equal(t, equivalent.SourceDigest, value.SourceDigest)
		value.Models[0].EnableGroups[0] = "changed"
		value.Models[0].SupportedEndpointTypes[0] = "changed"
		assert.Equal(t, "default", input[0].EnableGroup[1])
		assert.Equal(t, constant.EndpointTypeOpenAI, input[0].SupportedEndpointTypes[1])
	})
	t.Run("未知定价扩展不进入固定库存DTO", func(t *testing.T) {
		input := []model.Pricing{{ModelName: "bounded"}}
		baseline, err := build(input, nil, nil, nil, time.Unix(1700000000, 0))
		require.NoError(t, err)
		input[0].BillingUsageSchema = map[string]jsplugin.UsageFieldSchema{"future": {Type: "enum", Enum: []string{strings.Repeat("[", 30)}}}
		input[0].BillingUsageExamples = []jsplugin.UsageExample{{Label: "future", Facts: map[string]any{"nested": map[string]any{"extension": true}}}}
		input[0].PricingVersion = "future-version"
		value, err := build(input, nil, nil, nil, time.Unix(1700000000, 0))
		require.NoError(t, err)
		assert.Equal(t, baseline.SourceDigest, value.SourceDigest)
		encoded, err := value.EncodeResourceBounded()
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "billing_usage_schema")
		assert.NotContains(t, string(encoded), "billing_usage_examples")
		assert.NotContains(t, string(encoded), "pricing_version")
	})
	t.Run("嵌套集合先计数", func(t *testing.T) {
		for _, kind := range []string{"groups", "endpoints", "vendors", "ratios", "endpoint_map"} {
			t.Run(kind, func(t *testing.T) {
				input := []model.Pricing{{ModelName: "bounded"}}
				var vendors []model.PricingVendor
				var endpoints map[string]common.EndpointInfo
				var ratios map[string]float64
				switch kind {
				case "groups":
					for i := 0; i < 129; i++ {
						input[0].EnableGroup = append(input[0].EnableGroup, fmt.Sprint(i))
					}
				case "endpoints":
					for i := 0; i < 33; i++ {
						input[0].SupportedEndpointTypes = append(input[0].SupportedEndpointTypes, constant.EndpointType(fmt.Sprint(i)))
					}
				case "vendors":
					vendors = make([]model.PricingVendor, 257)
				case "ratios":
					ratios = map[string]float64{}
					for i := 0; i < 129; i++ {
						ratios[fmt.Sprint(i)] = 1
					}
				case "endpoint_map":
					endpoints = map[string]common.EndpointInfo{}
					for i := 0; i < 33; i++ {
						endpoints[fmt.Sprint(i)] = common.EndpointInfo{}
					}
				}
				inventory, err := build(input, vendors, endpoints, ratios, time.Unix(1700000000, 0))
				require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
				assert.Nil(t, inventory)
			})
		}
	})
}
