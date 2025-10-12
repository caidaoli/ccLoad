#!/bin/bash
# 自动修复目录重构后的导入错误

set -e

echo "🔧 开始修复导入语句..."

# 1. 添加internal/server/server.go的导入
echo "📝 修复 internal/server/server.go..."
if ! grep -q "ccLoad/internal/proxy" internal/server/server.go; then
    sed -i '' '/ccLoad\/internal\/storage\/sqlite/a\
    "ccLoad/internal/proxy"\
' internal/server/server.go
fi

# 2. 添加internal/server/admin.go的导入
echo "📝 修复 internal/server/admin.go..."
if ! grep -q "ccLoad/internal/util" internal/server/admin.go; then
    sed -i '' '/ccLoad\/internal\/storage\/sqlite/a\
    "ccLoad/internal/util"\
    "ccLoad/internal/testutil"\
' internal/server/admin.go
fi

# 3. 添加internal/server/handlers.go的导入
echo "📝 修复 internal/server/handlers.go..."
if ! grep -q "ccLoad/internal/util" internal/server/handlers.go; then
    sed -i '' '/ccLoad\/internal\/storage/a\
    "ccLoad/internal/util"\
' internal/server/handlers.go
fi

# 4. 修复internal/proxy中的引用
echo "📝 修复 internal/proxy/*.go..."
for file in internal/proxy/*.go; do
    if ! grep -q "ccLoad/internal/util" "$file" 2>/dev/null; then
        if grep -q "ccLoad/internal/" "$file"; then
            sed -i '' '/ccLoad\/internal\/model/a\
    "ccLoad/internal/util"\
' "$file" 2>/dev/null || true
        fi
    fi
done

# 5. 更新函数调用（添加包前缀）
echo "📝 更新函数调用前缀..."

# KeySelector -> proxy.KeySelector (只在server.go中)
sed -i '' 's/\*KeySelector/*proxy.KeySelector/g' internal/server/server.go
sed -i '' 's/NewKeySelector/proxy.NewKeySelector/g' internal/server/server.go

# 工具函数 -> util.xxx
for file in internal/server/*.go; do
    sed -i '' 's/\bParseAPIKeys\b/util.ParseAPIKeys/g' "$file"
    sed -i '' 's/\bIsValidChannelType\b/util.IsValidChannelType/g' "$file"
    sed -i '' 's/\bnormalizeChannelType\b/util.NormalizeChannelType/g' "$file"
done

# 测试器 -> testutil.xxx
for file in internal/server/*.go; do
    sed -i '' 's/\bChannelTester\b/testutil.ChannelTester/g' "$file"
    sed -i '' 's/\bCodexTester\b/testutil.CodexTester/g' "$file"
    sed -i '' 's/\bOpenAITester\b/testutil.OpenAITester/g' "$file"
done

echo "✅ 导入修复完成!"
echo ""
echo "🔨 尝试构建..."
if go build -o /tmp/ccload .; then
    echo "✅ 构建成功!"
    echo ""
    echo "🧪 运行测试..."
    go test ./... -v || echo "⚠️  部分测试失败，请检查"
else
    echo "❌ 构建失败，请查看上面的错误信息"
    echo ""
    echo "💡 提示：可能需要手动修复一些导入"
    exit 1
fi
