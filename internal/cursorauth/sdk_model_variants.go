package cursorauth

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
)

/**
 * @brief 分离可选的 Cursor 速度后缀。
 * @param model 已去除首尾空白的模型名。
 * @return 后缀前的模型选择；没有后缀时返回原名。
 * @return SDK fast 值；没有后缀时为空。
 */
func splitCursorSpeedSuffix(model string) (string, string) {
	lower := strings.ToLower(model)
	if len(model) > len("-fast") && strings.HasSuffix(lower, "-fast") {
		return strings.TrimSpace(model[:len(model)-len("-fast")]), "true"
	}
	if len(model) > len("-slow") && strings.HasSuffix(lower, "-slow") {
		return strings.TrimSpace(model[:len(model)-len("-slow")]), "false"
	}
	return model, ""
}

/**
 * @brief 补充可解析的速度和上下文别名及其能力。
 * @param catalog 当前凭证的模型目录；其中原生 ID 优先。
 * @param item 当前原生模型及其变体。
 * @param id 当前原生 ID，已去除首尾空白。
 * @param defaultWindow 原生模型的标称窗口；未知时为零。
 * @param add 收录可发现的别名及窗口的函数。
 * @param snapshot 写入别名的思考档位和已保存的默认窗口。
 */
func addCursorModelAliases(catalog []*sdkv1.SdkModel, item *sdkv1.SdkModel, id string, defaultWindow int, add func(string, int), snapshot *ModelCatalog) {
	addEfforts := func(alias string) {
		beforeSpeed, speed := splitCursorSpeedSuffix(alias)
		item, options := resolveCursorModelOptions(catalog, beforeSpeed, speed)
		snapshot.recordCursorEfforts(alias, item, options)
	}
	addAlias := func(alias string, window int) {
		if cursorCatalogModel(catalog, alias) == nil {
			add(alias, window)
			addEfforts(alias)
		}
	}
	keepAlias := func(alias string, window int) {
		if cursorCatalogModel(catalog, alias) == nil {
			if window > 0 {
				snapshot.ContextWindows[alias] = window
			}
			addEfforts(alias)
		}
	}
	fast, _ := cursorCatalogParamValue(item, "fast", "true")
	slow, _ := cursorCatalogParamValue(item, "fast", "false")
	lower := strings.ToLower(id)
	fastWindow := cursorSpeedContextWindow(item, "true", defaultWindow)
	slowWindow := cursorSpeedContextWindow(item, "false", defaultWindow)
	if fast != nil && !strings.HasSuffix(lower, "-fast") {
		addAlias(id+"-fast", fastWindow)
	}
	if slow != nil && !strings.HasSuffix(lower, "-slow") {
		if fast != nil && !strings.EqualFold(cursorDefaultVariantParam(item, "fast").GetValue(), "false") {
			addAlias(id+"-slow", slowWindow)
		} else {
			keepAlias(id+"-slow", slowWindow)
		}
	}
	for _, context := range cursorCatalogContexts(item) {
		alias := id + "@" + context
		if cursorCatalogModel(catalog, alias) != nil {
			continue
		}
		window := parseCursorContextWindow(context)
		add(alias, window)
		addEfforts(alias)
		if cursorContextSpeedParams(item, context, "true") != nil {
			addAlias(alias+"-fast", window)
		}
		if cursorContextSpeedParams(item, context, "false") != nil {
			addAlias(alias+"-slow", window)
		}
	}
	// Existing configurations may still use an explicit window that is now the default.
	context := cursorDefaultVariantParam(item, "context").GetValue()
	alias := id + "@" + context
	if context != "" && cursorCatalogModel(catalog, alias) == nil {
		keepAlias(alias, defaultWindow)
		if cursorContextSpeedParams(item, context, "true") != nil {
			window := 0
			if fastWindow == defaultWindow {
				window = defaultWindow
			}
			keepAlias(alias+"-fast", window)
		}
		if cursorContextSpeedParams(item, context, "false") != nil {
			window := 0
			if slowWindow == defaultWindow {
				window = defaultWindow
			}
			keepAlias(alias+"-slow", window)
		}
	}
}

/**
 * @brief 匹配 Cursor 目录中以 k 或 m 表示的标称上下文窗口。
 */
var cursorContextWindowPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([km])$`)

/**
 * @brief 将 Cursor 目录中的标称窗口转为 token 数。
 * @param context 目录参数值，如 272k 或 1.5m。
 * @return 十进制单位的 token 数；无法解析或超出范围时为零。
 */
func parseCursorContextWindow(context string) int {
	parts := cursorContextWindowPattern.FindStringSubmatch(strings.TrimSpace(context))
	if parts == nil {
		return 0
	}
	amount, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0
	}
	unit := 1000.0
	if strings.EqualFold(parts[2], "m") {
		unit = 1000000
	}
	window := math.Round(amount * unit)
	if window <= 0 || window >= float64(math.MaxInt) {
		return 0
	}
	return int(window)
}

/**
 * @brief 查询原生模型默认选择的标称窗口。
 * @param item SDK 模型目录项。
 * @return 默认变体或目录唯一可选窗口的 token 数；未知时为零。
 */
func cursorDefaultContextWindow(item *sdkv1.SdkModel) int {
	if context := cursorDefaultVariantParam(item, "context").GetValue(); context != "" {
		return parseCursorContextWindow(context)
	}
	if len(item.GetVariants()) == 0 {
		if contexts := cursorCatalogContexts(item); len(contexts) == 1 {
			return parseCursorContextWindow(contexts[0])
		}
	}
	return 0
}

/**
 * @brief 查询仅指定速度时可确定的标称窗口。
 * @param item SDK 模型目录项。
 * @param speed SDK fast 值。
 * @param defaultWindow 原生模型默认窗口；未知时为零。
 * @return 指定速度下的唯一窗口，或速度与默认一致时的默认窗口；未知时为零。
 */
func cursorSpeedContextWindow(item *sdkv1.SdkModel, speed string, defaultWindow int) int {
	if strings.EqualFold(cursorDefaultVariantParam(item, "fast").GetValue(), speed) {
		return defaultWindow
	}
	var window int
	for _, variant := range item.GetVariants() {
		if !strings.EqualFold(cursorVariantParam(variant, "fast").GetValue(), speed) {
			continue
		}
		current := parseCursorContextWindow(cursorVariantParam(variant, "context").GetValue())
		if current == 0 || (window != 0 && window != current) {
			return 0
		}
		window = current
	}
	return window
}

/**
 * @brief 提取目录声明的非默认上下文值，并保留上游拼写。
 * @param item SDK 模型目录项。
 * @return 去重后的可选上下文，使用目录顺序。
 */
func cursorCatalogContexts(item *sdkv1.SdkModel) []string {
	defaultContext := cursorDefaultVariantParam(item, "context").GetValue()
	seen := map[string]bool{"": true, strings.ToLower(defaultContext): true}
	var contexts []string
	add := func(context string) {
		key := strings.ToLower(context)
		if !seen[key] {
			seen[key] = true
			contexts = append(contexts, context)
		}
	}
	for _, parameter := range item.GetParameters() {
		if strings.EqualFold(parameter.GetId(), "context") {
			for _, value := range parameter.GetValues() {
				add(value.GetValue())
			}
		}
	}
	for _, variant := range item.GetVariants() {
		add(cursorVariantParam(variant, "context").GetValue())
	}
	return contexts
}

/**
 * @brief 读取默认变体中的参数。
 * @param item SDK 模型目录项。
 * @param id SDK 参数名。
 * @return 默认变体参数，未声明时为 nil；未标记变体时使用目录第一项。
 */
func cursorDefaultVariantParam(item *sdkv1.SdkModel, id string) *sdkv1.ModelParameterValue {
	variants := item.GetVariants()
	for _, variant := range variants {
		if variant.GetIsDefault() {
			return cursorVariantParam(variant, id)
		}
	}
	if len(variants) == 0 {
		return nil
	}
	return cursorVariantParam(variants[0], id)
}

/**
 * @brief 读取一个 SDK 变体中的具名参数。
 * @param variant SDK 目录中的变体。
 * @param id SDK 参数名。
 * @return 保留目录拼写的参数，未声明时为 nil。
 */
func cursorVariantParam(variant *sdkv1.ModelVariant, id string) *sdkv1.ModelParameterValue {
	for _, param := range variant.GetParams() {
		if strings.EqualFold(param.GetId(), id) {
			return param
		}
	}
	return nil
}

/**
 * @brief 查找目录实际声明的上下文和速度组合。
 * @param item SDK 模型目录项。
 * @param context 非默认上下文值。
 * @param speed 明确指定的 SDK fast 值。
 * @return 可发给 SDK 的参数；目录无此组合时返回 nil。
 */
func cursorContextSpeedParams(item *sdkv1.SdkModel, context, speed string) []*sdkv1.ModelParameterValue {
	for _, variant := range item.GetVariants() {
		window := cursorVariantParam(variant, "context")
		fast := cursorVariantParam(variant, "fast")
		if strings.EqualFold(window.GetValue(), context) && strings.EqualFold(fast.GetValue(), speed) {
			return []*sdkv1.ModelParameterValue{window, fast}
		}
	}
	return nil
}

/**
 * @brief 根据实时目录解析移除最外层速度后缀后的模型选择。
 * @param catalog 当前凭证的模型目录。
 * @param id 原生 ID 或附带上下文的模型 ID。
 * @param speed SDK fast 值，无明确速度时为空。
 * @return 目录中的模型；无法解析时为 nil。
 * @return 目录声明的上下文和速度参数（含显式指定的默认窗口）；原生模型不支持慢速时为 nil。
 */
func resolveCursorModelOptions(catalog []*sdkv1.SdkModel, id, speed string) (*sdkv1.SdkModel, []*sdkv1.ModelParameterValue) {
	if item := cursorCatalogModel(catalog, id); item != nil {
		if speed == "" {
			return item, nil
		}
		if param, _ := cursorCatalogParamValue(item, "fast", speed); param != nil {
			return item, []*sdkv1.ModelParameterValue{param}
		}
		if speed == "true" {
			// Explicit -fast aliases predate parameter declarations in the catalog.
			return item, []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}
		}
		return item, nil
	}
	separator := strings.LastIndexByte(id, '@')
	if separator <= 0 || separator == len(id)-1 {
		return nil, nil
	}
	item, context := cursorCatalogModel(catalog, id[:separator]), id[separator+1:]
	if speed != "" {
		if params := cursorContextSpeedParams(item, context, speed); params != nil {
			return item, params
		}
	} else if param, _ := cursorCatalogParamValue(item, "context", strings.ToLower(context)); param != nil {
		return item, []*sdkv1.ModelParameterValue{param}
	}
	return nil, nil
}

/**
 * @brief 收录模型选择的思考档位，仅在目录默认档受该选择支持时公布它。
 * @param id 公开的模型或变体 ID。
 * @param item 原生 SDK 目录项。
 * @param options 该选择的窗口和速度约束。
 */
func (snapshot *ModelCatalog) recordCursorEfforts(id string, item *sdkv1.SdkModel, options []*sdkv1.ModelParameterValue) {
	levels := cursorModelEffortLevels(item, options)
	snapshot.EffortLevels[id] = levels
	for _, variant := range item.GetVariants() {
		if variant.GetIsDefault() {
			effort := cursorSentEffort(&sdkv1.ModelSelection{Params: variant.GetParams()})
			if slices.Contains(levels, effort) {
				snapshot.DefaultEfforts[id] = effort
			}
			return
		}
	}
}

/**
 * @brief 计算实际模型选择支持的规范思考档位。
 * @param item 账号目录中的原生模型。
 * @param options 模型别名选择的上下文和速度。
 * @return 按档位递增排序的列表；空列表表示无可设档位。
 */
func cursorModelEffortLevels(item *sdkv1.SdkModel, options []*sdkv1.ModelParameterValue) []string {
	levels := []string{}
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if cursorEffortParams(item, options, level) != nil {
			levels = append(levels, level)
		}
	}
	return levels
}

/**
 * @brief 确认已选上下文、速度及思考参数能共同出现在 SDK 目录中的同一变体。
 * @param item 账号目录中的原生模型。
 * @param options 模型选择的上下文和速度。
 * @param efforts 发给 SDK 的思考参数。
 * @return 声明了组合，或目录仅在参数定义中列出相关档位时返回 true。
 */
func cursorVariantSupportsEffort(item *sdkv1.SdkModel, options []*sdkv1.ModelParameterValue, efforts ...*sdkv1.ModelParameterValue) bool {
	variants := item.GetVariants()
	var constraints []*sdkv1.ModelParameterValue
	for _, param := range slices.Concat(options, efforts) {
		if slices.ContainsFunc(variants, func(variant *sdkv1.ModelVariant) bool {
			return cursorVariantParam(variant, param.GetId()) != nil
		}) {
			constraints = append(constraints, param)
		}
	}
	if len(constraints) == 0 {
		return true
	}
	return slices.ContainsFunc(variants, func(variant *sdkv1.ModelVariant) bool {
		for _, param := range constraints {
			if !strings.EqualFold(cursorVariantParam(variant, param.GetId()).GetValue(), param.GetValue()) {
				return false
			}
		}
		return true
	})
}
