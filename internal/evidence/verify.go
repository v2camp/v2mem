package evidence

// VerifyRef 是归档回读的**生产级**证明（design-data-lifecycle §8.1 铁律入口）：
// 按 raw_ref（path#seq）真正把记录读回手里，并校验 archive_hash。
// 任何一步失败都返回错误 —— 调用方（GC）据此拒绝扩大删除范围：
// 没有证明能读回之前，不许扩大自动物理 GC。
func VerifyRef(locator, wantHash string) (RawEvent, error) {
	if err := RefHealth(locator, wantHash); err != nil {
		return RawEvent{}, err
	}
	return ReadArchiveRecord(locator)
}
