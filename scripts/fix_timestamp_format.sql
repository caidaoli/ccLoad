-- 修复channels表时间戳格式问题
-- 问题：created_at/updated_at字段存储为字符串而非Unix时间戳（BIGINT）
-- 场景：CSV导入、直接SQL INSERT等操作可能导致此问题
-- 执行：sqlite3 data/ccload.db < scripts/fix_timestamp_format.sql

-- 1. 检查问题
SELECT
  '== 检测到问题渠道数量 ==' as step,
  COUNT(*) as count
FROM channels
WHERE typeof(created_at) = 'text'
   OR typeof(updated_at) = 'text';

-- 2. 修复时间戳（转换为当前Unix时间戳）
UPDATE channels
SET
  created_at = CAST(strftime('%s', 'now') AS INTEGER),
  updated_at = CAST(strftime('%s', 'now') AS INTEGER)
WHERE
  typeof(created_at) = 'text'
  OR typeof(updated_at) = 'text';

-- 3. 验证修复结果
SELECT
  '== 验证修复结果 ==' as step,
  typeof(created_at) as created_at_type,
  typeof(updated_at) as updated_at_type,
  COUNT(*) as count
FROM channels
GROUP BY typeof(created_at), typeof(updated_at);

-- 4. 显示修复后的样例数据
SELECT
  '== 样例渠道数据 ==' as step,
  id,
  name,
  created_at,
  updated_at,
  datetime(created_at, 'unixepoch') as created_at_readable
FROM channels
LIMIT 3;
