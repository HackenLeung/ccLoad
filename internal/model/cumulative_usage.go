package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// ClientNameMaxLen 客户端软件标识列宽上限（与 logs.client_name、cumulative_usage.client_name 的 VARCHAR 长度对齐）。
// 累计统计的维度 key 与日志落库共用此上限：两处截断不一致会让同一维度分裂成多行。
const ClientNameMaxLen = 64

// TruncateColumnValue 按字节截断字段值到数据库列宽，避免超长值触发 MySQL strict mode 写入失败。
func TruncateColumnValue(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

// CumulativeUsageDimensionKey 计算累计统计的维度哈希（SHA256 十六进制，64 字符）。
//
// 各字段按「8 字节长度前缀 + 内容」写入哈希，避免不同字段拼接产生歧义
// （如 model="ab", logSource="c" 与 model="a", logSource="bc" 撞成同一 key）。
// 调用方传入的 clientName 必须已截断到 ClientNameMaxLen，与落库值保持一致。
func CumulativeUsageDimensionKey(channelID int64, modelName string, statusCode int, authTokenID int64, clientName, logSource string) string {
	hash := sha256.New()
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(channelID))
	_, _ = hash.Write(number[:])
	writeString := func(value string) {
		binary.BigEndian.PutUint64(number[:], uint64(len(value)))
		_, _ = hash.Write(number[:])
		_, _ = hash.Write([]byte(value))
	}
	writeString(modelName)
	binary.BigEndian.PutUint64(number[:], uint64(statusCode))
	_, _ = hash.Write(number[:])
	binary.BigEndian.PutUint64(number[:], uint64(authTokenID))
	_, _ = hash.Write(number[:])
	writeString(clientName)
	writeString(logSource)
	return hex.EncodeToString(hash.Sum(nil))
}
