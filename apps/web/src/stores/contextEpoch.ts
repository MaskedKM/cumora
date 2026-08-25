/** Commit an async result only while the context that started it is current. */
export async function commitIfEpochCurrent<T>(
  getEpoch: () => number,
  request: () => Promise<T>,
  commit: (value: T) => void,
): Promise<boolean> {
  const epoch = getEpoch()
  const value = await request()
  if (getEpoch() !== epoch) return false
  commit(value)
  return true
}
