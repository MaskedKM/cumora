// 我的视图桶(#219 ④)共享件 —— 从 MeView.tsx 原样搬移:Section(节标题+卡容器
// 的两段式外壳),profile/usage/trust/projects/preferences/computers 六件共用。
export function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <h4 className="text-[10.5px] font-extrabold text-skype tracking-[0.14em] uppercase mb-3">{title}</h4>
      {children}
    </div>
  )
}
