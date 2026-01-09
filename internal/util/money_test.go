package util

import (
	"math"
	"testing"
)

func TestUSDToMicroUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		usd      float64
		expected int64
	}{
		{"zero", 0, 0},
		{"one dollar", 1.0, 1_000_000},
		{"one cent", 0.01, 10_000},
		{"one micro", 0.000001, 1},
		{"sub micro rounds to zero", 0.0000001, 0},
		{"typical cost", 0.0015, 1500},
		{"large value", 999.999999, 999_999_999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := USDToMicroUSD(tt.usd)
			if result != tt.expected {
				t.Errorf("USDToMicroUSD(%v) = %d, want %d", tt.usd, result, tt.expected)
			}
		})
	}
}

func TestUSDToMicroUSD_InvalidInputs(t *testing.T) {
	t.Parallel()

	// 非法输入应该返回0而不是panic
	tests := []struct {
		name string
		usd  float64
	}{
		{"negative", -1.0},
		{"negative small", -0.000001},
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := USDToMicroUSD(tt.usd)
			if result != 0 {
				t.Errorf("USDToMicroUSD(%v) = %d, want 0 for invalid input", tt.usd, result)
			}
		})
	}
}

func TestUSDToMicroUSDSafe(t *testing.T) {
	t.Parallel()

	// 测试正常值
	t.Run("valid values", func(t *testing.T) {
		result, err := USDToMicroUSDSafe(1.0)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if result != 1_000_000 {
			t.Errorf("USDToMicroUSDSafe(1.0) = %d, want 1000000", result)
		}
	})

	// 测试非法值返回error
	t.Run("negative returns error", func(t *testing.T) {
		_, err := USDToMicroUSDSafe(-1.0)
		if err == nil {
			t.Error("USDToMicroUSDSafe(-1.0) should return error")
		}
	})

	t.Run("NaN returns error", func(t *testing.T) {
		_, err := USDToMicroUSDSafe(math.NaN())
		if err == nil {
			t.Error("USDToMicroUSDSafe(NaN) should return error")
		}
	})

	t.Run("Inf returns error", func(t *testing.T) {
		_, err := USDToMicroUSDSafe(math.Inf(1))
		if err == nil {
			t.Error("USDToMicroUSDSafe(Inf) should return error")
		}
	})

	t.Run("zero returns no error", func(t *testing.T) {
		result, err := USDToMicroUSDSafe(0)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if result != 0 {
			t.Errorf("USDToMicroUSDSafe(0) = %d, want 0", result)
		}
	})
}

func TestMicroUSDToUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		microUSD int64
		expected float64
	}{
		{"zero", 0, 0},
		{"one million", 1_000_000, 1.0},
		{"ten thousand", 10_000, 0.01},
		{"one", 1, 0.000001},
		{"typical", 1500, 0.0015},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MicroUSDToUSD(tt.microUSD)
			if result != tt.expected {
				t.Errorf("MicroUSDToUSD(%d) = %v, want %v", tt.microUSD, result, tt.expected)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	// 测试往返转换的精度
	values := []float64{0, 0.01, 0.001, 0.0001, 0.00001, 0.000001, 1.0, 10.0, 100.0}
	for _, v := range values {
		micro := USDToMicroUSD(v)
		back := MicroUSDToUSD(micro)
		// 允许微小误差（因为四舍五入）
		diff := math.Abs(v - back)
		if diff > 0.0000005 { // 半微美元的误差
			t.Errorf("roundtrip(%v): got %v, diff=%v", v, back, diff)
		}
	}
}
