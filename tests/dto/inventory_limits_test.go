package dtotests_test

import (
	"math"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inventory() *dto.LlmModelInventory {
	return &dto.LlmModelInventory{
		SourceRevision: math.MaxInt64,
		SourceDigest:   strings.Repeat("a", 64),
		MeasuredAt:     "2026-10-14T12:00:00Z",
		LlmModelInventorySnapshot: dto.LlmModelInventorySnapshot{
			Models:            []dto.LlmModelInventoryEntry{{ModelName: "中文<&模型", SupportedEndpointTypes: []string{"openai"}, EnableGroups: []string{"default"}}},
			Vendors:           []dto.LlmInventoryVendor{{VendorId: math.MaxInt64, Name: "供应商"}},
			GroupRatio:        map[string]float64{"带<&键": 0.5},
			SupportedEndpoint: map[string]common.EndpointInfo{"openai": {Path: "/v1/中文<&", Method: "POST"}},
		},
	}
}

func TestInventoryEncodedLimitUsesActualJSONBytes(t *testing.T) {
	cases := []struct {
		name    string
		prepare func() *dto.LlmModelInventory
		setText func(*dto.LlmModelInventory, string)
	}{
		{"供应商说明及所有集合逗号", func() *dto.LlmModelInventory {
			v := inventory()
			v.Models = append(v.Models, dto.LlmModelInventoryEntry{ModelName: "second"})
			v.Vendors = append(v.Vendors, dto.LlmInventoryVendor{VendorId: 2, Name: "second"})
			v.GroupRatio["第二个组"] = 2
			v.SupportedEndpoint["second"] = common.EndpointInfo{Path: "/v2"}
			return v
		}, func(v *dto.LlmModelInventory, text string) { v.Vendors[1].Description = text }},
		{"map键转义", inventory, func(v *dto.LlmModelInventory, text string) {
			v.GroupRatio = map[string]float64{"前缀<&\"中" + text: 0.5, "second": 2}
		}},
		{"端点map值转义", inventory, func(v *dto.LlmModelInventory, text string) {
			v.SupportedEndpoint["带<&\"键"] = common.EndpointInfo{Path: "前缀<&\"中" + text, Method: "POST"}
		}},
		{"nil集合骨架", func() *dto.LlmModelInventory { return &dto.LlmModelInventory{} }, func(v *dto.LlmModelInventory, text string) { v.MeasuredAt = text }},
		{"empty集合骨架", func() *dto.LlmModelInventory {
			return &dto.LlmModelInventory{LlmModelInventorySnapshot: dto.LlmModelInventorySnapshot{
				Models: []dto.LlmModelInventoryEntry{}, Vendors: []dto.LlmInventoryVendor{},
				GroupRatio: map[string]float64{}, SupportedEndpoint: map[string]common.EndpointInfo{},
			}}
		}, func(v *dto.LlmModelInventory, text string) { v.SourceDigest = text }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.prepare()
			tc.setText(value, "x")
			base, err := common.Marshal(value)
			require.NoError(t, err)
			padding := 16*1024*1024 - len(base) + 1
			tc.setText(value, strings.Repeat("x", padding))
			require.NoError(t, value.ValidateResourceLimits())
			encoded, err := value.EncodeResourceBounded()
			require.NoError(t, err)
			assert.Len(t, encoded, 16*1024*1024, "大整数、UTF8、HTML转义、逗号与map键开销必须计入精确最终界")
			tc.setText(value, strings.Repeat("x", padding+1))
			require.ErrorIs(t, value.ValidateResourceLimits(), dto.ErrLlmInventoryLimits)
			encoded, err = value.EncodeResourceBounded()
			require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
			assert.Nil(t, encoded, "越界不能交付部分成功JSON")
		})
	}
}

func TestInventoryFinalRevisionGrowthIsCountedBeforeEncoding(t *testing.T) {
	value := inventory()
	value.SourceRevision = 0
	value.Vendors[0].Description = "x"
	base, err := common.Marshal(value)
	require.NoError(t, err)
	value.Vendors[0].Description = strings.Repeat("x", 16*1024*1024-len(base)+1)
	encoded, err := value.EncodeResourceBounded()
	require.NoError(t, err)
	assert.Len(t, encoded, 16*1024*1024)
	value.SourceRevision = math.MaxInt64
	encoded, err = value.EncodeResourceBounded()
	require.ErrorIs(t, err, dto.ErrLlmInventoryLimits, "实际revision比预检的0多18字节，不能越过最终门")
	assert.Nil(t, encoded)
}

func TestInventoryEncodingPreservesNilAndEmptyCollections(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "empty"}[empty], func(t *testing.T) {
			value := &dto.LlmModelInventory{SourceRevision: 42, SourceDigest: "a", MeasuredAt: "时间"}
			if empty {
				value.Models = []dto.LlmModelInventoryEntry{}
				value.Vendors = []dto.LlmInventoryVendor{}
				value.GroupRatio = map[string]float64{}
				value.SupportedEndpoint = map[string]common.EndpointInfo{}
			}
			want, err := common.Marshal(value)
			require.NoError(t, err)
			got, err := value.EncodeResourceBounded()
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}
}

func TestInventoryResourceRejectionsAreWholeSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*dto.LlmModelInventory)
	}{
		{"模型名UTF8字节", func(v *dto.LlmModelInventory) { v.Models[0].ModelName = strings.Repeat("中", 43) }},
		{"模型名无效UTF8", func(v *dto.LlmModelInventory) { v.Models[0].ModelName = string([]byte{0xff}) }},
		{"单模型巨大字符串", func(v *dto.LlmModelInventory) { v.Models[0].Description = strings.Repeat("x", 4097) }},
		{"整包巨大说明", func(v *dto.LlmModelInventory) { v.Vendors[0].Description = strings.Repeat("x", 16*1024*1024+1) }},
		{"map巨大键", func(v *dto.LlmModelInventory) {
			v.GroupRatio = map[string]float64{strings.Repeat("x", 16*1024*1024+1): 1}
		}},
		{"endpoint巨大值", func(v *dto.LlmModelInventory) {
			v.SupportedEndpoint = map[string]common.EndpointInfo{"openai": {Path: strings.Repeat("x", 16*1024*1024+1)}}
		}},
		{"source巨大说明", func(v *dto.LlmModelInventory) { v.MeasuredAt = strings.Repeat("x", 16*1024*1024+1) }},
		{"端点集合越界", func(v *dto.LlmModelInventory) { v.Models[0].SupportedEndpointTypes = make([]string, 33) }},
		{"分组集合越界", func(v *dto.LlmModelInventory) { v.Models[0].EnableGroups = make([]string, 129) }},
		{"模型数越界", func(v *dto.LlmModelInventory) { v.Models = make([]dto.LlmModelInventoryEntry, 4097) }},
		{"供应商数越界", func(v *dto.LlmModelInventory) { v.Vendors = make([]dto.LlmInventoryVendor, 257) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := inventory()
			tc.mutate(v)
			encoded, err := v.EncodeResourceBounded()
			require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
			assert.Nil(t, encoded)
		})
	}
	var absent *dto.LlmModelInventory
	_, err := absent.EncodeResourceBounded()
	require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
}

func TestInventoryNonFiniteFactsAreEncodingErrors(t *testing.T) {
	for _, field := range []string{"group_ratio", "model_ratio"} {
		t.Run(field, func(t *testing.T) {
			v := inventory()
			if field == "group_ratio" {
				v.GroupRatio["default"] = math.NaN()
			} else {
				v.Models[0].BillingFacts.ModelRatio = math.Inf(1)
			}
			encoded, err := v.EncodeResourceBounded()
			require.Error(t, err)
			assert.NotErrorIs(t, err, dto.ErrLlmInventoryLimits, "非有限计费事实是编码错误，不伪称资源超限")
			assert.Nil(t, encoded)
		})
	}
}

func TestInventoryModelNameLimitIsUTF8Bytes(t *testing.T) {
	v := inventory()
	v.Models[0].ModelName = strings.Repeat("中", 42) + "ab"
	encoded, err := v.EncodeResourceBounded()
	require.NoError(t, err)
	assert.Contains(t, string(encoded), v.Models[0].ModelName)
	v.Models[0].ModelName += "x"
	_, err = v.EncodeResourceBounded()
	require.ErrorIs(t, err, dto.ErrLlmInventoryLimits)
}

func TestInventoryFixedDTOCannotInterpretDescriptionAsNestedJSON(t *testing.T) {
	v := inventory()
	v.Models[0].Description = strings.Repeat("[", 30) + "{\"unknown\":true}" + strings.Repeat("]", 30)
	encoded, err := v.EncodeResourceBounded()
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, common.Unmarshal(encoded, &parsed))
	models := parsed["models"].([]any)
	entry := models[0].(map[string]any)
	assert.Equal(t, v.Models[0].Description, entry["description"])
	assert.NotContains(t, entry, "unknown", "说明正文不是新的协议扩展对象")
}
