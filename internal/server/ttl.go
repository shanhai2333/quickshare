package server

import (
	"strconv"
	"strings"
	"time"

	"quickshare/internal/store"
)

// ---------------------------------------------------------------- 保留时长
//
// 这一份管着三档自动清理规则，外加"单条覆盖"：
//
//	单条 ttl_seconds  >  验证码规则（只对文本）  >  全局设置
//
// 三档设置都是"数值 + 单位"存原样，不是统一换算成秒存起来：后者回填到界面时
// 要做反算（43200 分钟到底显示成 30 天还是 720 小时），换算边界上会出现
// "用户填 24 小时、回来变成 1 天"这种困惑。存原样就没这个问题。

// TTL 的取值上限。数值本身不大（最多 10000），真正的约束是别让人填一个
// 天文数字把 time.Duration 乘溢出。
const maxTTLValue = 10000

// 单条覆盖的上限，按"秒"算。跟全局那档的语义对齐（10000 天）。
const maxItemTTLSeconds = maxTTLValue * 24 * 60 * 60

// 验证码文本的默认保留时长。用户没设过这个键时用它。
//
// 注意它和"显式设成 0"是两回事：后者表示**关掉验证码规则**，让验证码按
// 普通文本走。所以读设置时必须区分"键不存在"和"键存在且为 0"。
const defaultCodeTTL = 10 * time.Minute

// 验证码判定的长度上限（字符）。只对"关键词"那条分支生效。
//
// 为什么要卡：`code` 这种词在代码、报错、文档里到处都是，不卡长度的话
// 一段贴进来的源码（含 code 和 4 位以上数字）就会被当成验证码，10 分钟后
// 被悄悄删掉。往"少删"的方向偏——漏判只是没享受到短保留，误判是丢数据。
const codeTextMaxLen = 200

// ttlPolicy 是当前设置折算出来的一套保留规则。0 一律表示"这一档不生效"。
type ttlPolicy struct {
	text time.Duration // 普通文本
	code time.Duration // 验证码文本；0 = 不特殊处理，按普通文本走
	file time.Duration // 文件
}

// policyFrom 把设置读成一套规则。
//
// 三个 From 返回的 bool 是"这档启用了吗"，这里刻意**不**把 false 和 0 区分开：
// 两者对"删不删"的效果完全一样，分开存只会多一个要同步的状态。
func policyFrom(kv map[string]string) ttlPolicy {
	text, _ := textTTLFrom(kv)
	code, _ := codeTTLFrom(kv)
	file, _ := fileTTLFrom(kv)
	return ttlPolicy{text: text, code: code, file: file}
}

// loadTTLPolicy 读设置并折算成一套保留规则。列表接口一次读一份，逐条复用。
func (s *Server) loadTTLPolicy() (ttlPolicy, error) {
	kv, err := s.be().st.GetSettings()
	if err != nil {
		return ttlPolicy{}, err
	}
	return policyFrom(kv), nil
}

// textExpiry 返回某条文本的到期时刻（unix 秒）。0 表示永不删除。
func (p ttlPolicy) textExpiry(t *store.Text) int64 {
	d := p.text
	if t.IsCode && p.code > 0 {
		d = p.code
	}
	if t.TTLSeconds > 0 {
		d = time.Duration(t.TTLSeconds) * time.Second
	}
	if d <= 0 {
		return 0
	}
	return t.CreatedAt.Add(d).Unix()
}

// fileExpiry 返回某个文件的到期时刻（unix 秒）。0 表示永不删除。
func (p ttlPolicy) fileExpiry(f *store.File) int64 {
	d := p.file
	if f.TTLSeconds > 0 {
		d = time.Duration(f.TTLSeconds) * time.Second
	}
	if d <= 0 {
		return 0
	}
	return f.CreatedAt.Add(d).Unix()
}

// textSeconds / codeSeconds / fileSeconds 给清理用。
func (p ttlPolicy) textSeconds() int64 { return int64(p.text / time.Second) }
func (p ttlPolicy) codeSeconds() int64 { return int64(p.code / time.Second) }
func (p ttlPolicy) fileSeconds() int64 { return int64(p.file / time.Second) }

// unitDuration 把"数值 + 单位"换算成时长。unit 为空时用 defUnit。
//
// 单位认不出来就返回 false，调用方一律当"没设置"——前端只会写这三个值，
// 走到这儿说明库里的值被人手改脏了，那就不动数据。**宁可留着让用户手动删，
// 也不要因为一个解析不了的值就把人家攒的东西清空。**
func unitDuration(n int, unit, defUnit string) (time.Duration, bool) {
	if unit == "" {
		// 只填了数值、没带单位（手写的请求）时用默认单位，别让它静默地不生效
		unit = defUnit
	}
	switch unit {
	case "minute":
		return time.Duration(n) * time.Minute, true
	case "hour":
		return time.Duration(n) * time.Hour, true
	case "day":
		return time.Duration(n) * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// ttlFrom 从设置里解析一档保留时长。bool 为 false 表示没启用自动清理
// ——值为空、解析不了、或者 <= 0 都算没启用。
func ttlFrom(kv map[string]string, valueKey, unitKey, defUnit string) (time.Duration, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(kv[valueKey]))
	if err != nil || n <= 0 {
		return 0, false
	}
	return unitDuration(n, kv[unitKey], defUnit)
}

// textTTLFrom 解析普通文本的保留时长。没设 = 永不自动删除。
func textTTLFrom(kv map[string]string) (time.Duration, bool) {
	return ttlFrom(kv, store.SettingTextTTLValue, store.SettingTextTTLUnit, "day")
}

// fileTTLFrom 解析文件的保留时长。没设 = 永不自动删除。
func fileTTLFrom(kv map[string]string) (time.Duration, bool) {
	return ttlFrom(kv, store.SettingFileTTLValue, store.SettingFileTTLUnit, "day")
}

// codeTTLFrom 解析验证码文本的保留时长。
//
// 跟上面两个有一处**语义差别**：键不存在时给默认值 10 分钟（不是"不启用"），
// 而显式设成 0 表示关掉这条规则。所以先看键在不在，不能直接走 ttlFrom。
func codeTTLFrom(kv map[string]string) (time.Duration, bool) {
	if strings.TrimSpace(kv[store.SettingCodeTTLValue]) == "" {
		return defaultCodeTTL, true
	}
	return ttlFrom(kv, store.SettingCodeTTLValue, store.SettingCodeTTLUnit, "minute")
}

// ---------------------------------------------------------------- 验证码识别

// codeKeywords 是"关键词"那条分支认的词。全部按小写比较（中文不受影响）。
var codeKeywords = []string{
	"验证码", "校验码", "动态码", "安全码", "短信码",
	"verification code", "one-time", "passcode", "otp", "code",
}

// looksLikeCode 判断一条文本像不像验证码。
//
// 两条分支，任一命中即算：
//
//  1. **整条就是 4~8 位数字**（首尾空白不算）。用户从短信里复制出来的就是这种。
//  2. 内容不超过 codeTextMaxLen，且含关键词、且含至少 4 位连续数字。
//
// 为什么不只看纯数字：短信原文往往是"【某某】验证码 123456，5 分钟内有效"，
// 用户会把整句贴过来，只认纯数字就漏了。为什么不只认关键词：用户更常直接
// 复制那串数字。两条都要。
//
// 判定结果在插入时就算好存进 is_code（见 store），所以这个函数只在写入口
// 调用一次——清理是每 10 分钟一轮的全表扫描，内容写定后不会变，重算是白烧 CPU。
func looksLikeCode(content string) bool {
	t := strings.TrimSpace(content)
	if isAllDigits(t) && len(t) >= 4 && len(t) <= 8 {
		return true
	}
	if len(t) > codeTextMaxLen {
		return false
	}
	low := strings.ToLower(t)
	hit := false
	for _, kw := range codeKeywords {
		if strings.Contains(low, kw) {
			hit = true
			break
		}
	}
	return hit && hasDigitRun(t, 4)
}

// isAllDigits 判断是不是一串纯数字（含长度 0 的情况）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hasDigitRun 判断有没有至少 n 位连续数字。
func hasDigitRun(s string, n int) bool {
	run := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			run++
			if run >= n {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}
