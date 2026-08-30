// observabilitycontract —— 别名 glue:server 生成物裸引用的根包 types 符号
// (SessionBearerScopes 常量与 *Params 查询参数结构)。由
// contract-gen-go.sh 生成,勿手改。
package observabilitycontract

import "github.com/MaskedKM/cumora/apps/server-go/internal/contract"

type GetAgentRunsParams = contract.GetAgentRunsParams
type GetTriageEconomicsParams = contract.GetTriageEconomicsParams

const SessionBearerScopes = contract.SessionBearerScopes
