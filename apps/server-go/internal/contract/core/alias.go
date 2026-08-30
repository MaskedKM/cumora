// corecontract —— 别名 glue:server 生成物裸引用的根包 types 符号
// (SessionBearerScopes 常量与 *Params 查询参数结构)。由
// contract-gen-go.sh 生成,勿手改。
package corecontract

import "github.com/MaskedKM/cumora/apps/server-go/internal/contract"

type AuthStartParams = contract.AuthStartParams
type SearchParams = contract.SearchParams

const SessionBearerScopes = contract.SessionBearerScopes
