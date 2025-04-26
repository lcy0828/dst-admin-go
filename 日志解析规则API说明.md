# 日志解析规则API接口说明

## API接口列表

### 1. 获取所有日志提取规则

**请求方式**：GET  
**URL**：`/api/gamelog/parser/rules`  
**功能**：获取所有已配置的日志提取规则  
**返回示例**：
```json
{
  "status": 200,
  "msg": "获取规则列表成功",
  "data": [
    {
      "id": 1,
      "name": "服务器启动",
      "description": "匹配服务器启动信息",
      "log_type": "system",
      "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\]: Starting Up",
      "is_regex": true,
      "is_enabled": true,
      "priority": 100,
      "match_mode": "single",
      "tail_pattern": "",
      "created_at": "2023-01-01T12:00:00Z",
      "updated_at": "2023-01-01T12:00:00Z"
    }
  ]
}
```

### 2. 添加日志提取规则

**请求方式**：POST  
**URL**：`/api/gamelog/parser/rules`  
**功能**：添加新的日志提取规则  
**请求参数**：

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| name | string | 是 | 规则名称 |
| description | string | 否 | 规则描述 |
| log_type | string | 是 | 日志类型，如"system", "chat", "player"等 |
| pattern | string | 是 | 匹配模式（正则表达式或字符串） |
| is_regex | boolean | 否 | 是否使用正则表达式，默认false |
| is_enabled | boolean | 否 | 是否启用，默认true |
| priority | integer | 否 | 优先级（数字越大优先级越高），默认50 |
| match_mode | string | 否 | 匹配模式："single"(单行), "multi_line"(多行), "head_tail"(首尾行)，默认"single" |
| tail_pattern | string | 否 | 尾行匹配模式（仅当match_mode为"head_tail"时有效） |

**请求示例**：
```json
{
  "name": "错误日志多行匹配",
  "description": "匹配多行错误日志",
  "log_type": "error",
  "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\].*?Error:",
  "is_regex": true,
  "is_enabled": true,
  "priority": 80,
  "match_mode": "multi_line"
}
```

**首尾行匹配示例**：
```json
{
  "name": "异常堆栈跟踪",
  "description": "匹配异常及其堆栈跟踪",
  "log_type": "error",
  "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\].*?Exception:",
  "is_regex": true,
  "is_enabled": true,
  "priority": 90,
  "match_mode": "head_tail",
  "tail_pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\].*?at .*\\)$"
}
```

**返回示例**：
```json
{
  "status": 200,
  "msg": "添加规则成功"
}
```

### 3. 更新日志提取规则

**请求方式**：PUT  
**URL**：`/api/gamelog/parser/rules/:id`  
**功能**：更新已有的日志提取规则  
**URL参数**：

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| id | integer | 是 | 规则ID |

**请求参数**：与添加规则相同

**请求示例**：
```json
{
  "name": "更新后的规则名称",
  "description": "更新后的规则描述",
  "log_type": "system",
  "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\]: Updated pattern",
  "is_regex": true,
  "is_enabled": true,
  "priority": 85,
  "match_mode": "multi_line",
  "tail_pattern": ""
}
```

**返回示例**：
```json
{
  "status": 200,
  "msg": "更新规则成功"
}
```

### 4. 删除日志提取规则

**请求方式**：DELETE  
**URL**：`/api/gamelog/parser/rules/:id`  
**功能**：删除指定的日志提取规则  
**URL参数**：

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| id | integer | 是 | 规则ID |

**返回示例**：
```json
{
  "status": 200,
  "msg": "删除规则成功"
}
```

## 匹配模式说明

系统支持三种日志匹配模式：

### 1. 单行匹配模式（single）

- 默认模式
- 每行日志单独匹配和处理
- 适用于大多数常规日志

### 2. 多行匹配模式（multi_line）

- 当匹配到一条日志后，继续向下匹配
- 如果下一条日志为空，则直接提交当前匹配结果
- 如果下一条日志不为空，且匹配规则类型与首条日志相同，则合并为一条，以换行符拼接
- 适用于需要合并相同类型连续日志的场景，如错误日志、警告日志等

### 3. 首尾行匹配模式（head_tail）

- 需要提供首行（pattern）和尾行（tail_pattern）的正则规则
- 当匹配到首行时，会继续向下匹配，直到匹配到第一个符合尾行规则的行
- 将首行到尾行之间的所有内容以换行符拼接，作为一条完整日志
- 适用于异常堆栈跟踪、多行JSON日志等有明确开始和结束标记的日志

## 使用场景示例

### 多行匹配模式示例

适用于需要将连续的同类型日志合并为一条的场景：

```
[12:34:56]: Error: Failed to load resource
[12:34:57]: Error: Connection timeout
[12:34:58]: Error: Retry attempt 1 failed
```

使用多行匹配模式后，上述三条日志会被合并为一条：

```
[12:34:56]: Error: Failed to load resource
[12:34:57]: Error: Connection timeout
[12:34:58]: Error: Retry attempt 1 failed
```

### 首尾行匹配模式示例

适用于有明确开始和结束标记的多行日志，如异常堆栈：

```
[12:34:56]: Exception: NullReferenceException
[12:34:56]:   at Function1() in File1.cs:line 10
[12:34:56]:   at Function2() in File2.cs:line 20
[12:34:56]:   at Function3() in File3.cs:line 30
[12:34:57]: System operation continues...
```

使用首尾行匹配模式（首行匹配"Exception:"，尾行匹配"at .*)"）后，异常堆栈会被合并为一条日志：

```
[12:34:56]: Exception: NullReferenceException
[12:34:56]:   at Function1() in File1.cs:line 10
[12:34:56]:   at Function2() in File2.cs:line 20
[12:34:56]:   at Function3() in File3.cs:line 30
```

而后续的系统操作日志会作为单独的日志处理。
