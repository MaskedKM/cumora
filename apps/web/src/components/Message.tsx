/**
 * Message 行渲染的拆分桶(#147 ③):实现按职责分居四件 ——
 *   ./message/richBody     富文本(mention/ref chip + CodeBlock + Markdown)
 *   ./message/artifactCards 文档/看板/卡片/日历 artifact 卡 + 工具/附件卡
 *   ./message/reactions    反应簇(pill/burst/快捷/tooltip)
 *   ./message/MessageRow   行调度层(System/Quote/Whisper/MessageRow/TypingRow)
 * 本文件保持原 import 路径的 re-export 面(desktop/mobile/ObservabilityView
 * 等消费方零改动)。memo 包装在实现文件原样搬运,未二次包装(#143 语义)。
 */
export { RichBody, CodeBlock } from './message/richBody'
export { SystemRow, MessageRow, TypingRow } from './message/MessageRow'
