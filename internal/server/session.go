package server

import (
	"sync"
	"time"

	"github.com/RexKang/quotaclock/internal/crypto"
	"github.com/RexKang/quotaclock/internal/logx"
)

// Sessions 管理员会话：HKDF 签名的 {sid, exp} 无状态令牌（重启不失效）+ 内存拒绝名单
// （运行期内登出立即失效；重启名单清空——「复活至 TTL 自然过期」尾巴已在 PRD r2 接受）。
type Sessions struct {
	key []byte

	mu       sync.Mutex
	rejected map[string]int64 // sid → exp（unix 秒）

	clock func() time.Time
}

// NewSessions 用派生密钥构造。
func NewSessions(key []byte) *Sessions {
	return &Sessions{key: key, rejected: map[string]int64{}, clock: time.Now}
}

// Issue 签发新会话，返回 cookie 值。cookie 值登记为敏感字面量（脱敏红线）。
func (s *Sessions) Issue() (string, error) {
	sid, err := crypto.NewSID()
	if err != nil {
		return "", err
	}
	exp := s.clock().Add(crypto.SessionTTL)
	val, err := crypto.SignSession(s.key, crypto.SessionClaims{SID: sid, Exp: exp.Unix()})
	if err != nil {
		return "", err
	}
	logx.RegisterSecret(val)
	return val, nil
}

// Verify 校验 cookie 值（签名/exp/格式），错误按 UNAUTHENTICATED/SESSION_INVALID 分型。
func (s *Sessions) Verify(value string) (crypto.SessionClaims, error) {
	return crypto.VerifySession(s.key, value)
}

// Revoke 登出：sid 进内存拒绝名单（插入时顺带清理已过期项，量级恒小）。
func (s *Sessions) Revoke(sid string, exp int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock().Unix()
	for k, e := range s.rejected {
		if e <= now {
			delete(s.rejected, k)
		}
	}
	s.rejected[sid] = exp
}

// Revoked sid 是否在拒绝名单。
func (s *Sessions) Revoked(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rejected[sid]
	return ok
}
