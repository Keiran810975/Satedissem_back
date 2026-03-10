package protocol

// -------------------------------------------------------------------
// SetStorage — 基于 map 的简单存储策略
// -------------------------------------------------------------------

// SetStorage 使用 map[int]bool 存储分片，适用于大多数场景
type SetStorage struct {
	fragments map[int]bool
}

// NewSetStorage 创建 SetStorage
func NewSetStorage() *SetStorage {
	return &SetStorage{
		fragments: make(map[int]bool),
	}
}

func (s *SetStorage) Has(fragmentID int) bool {
	return s.fragments[fragmentID]
}

func (s *SetStorage) Store(fragmentID int) {
	s.fragments[fragmentID] = true
}

func (s *SetStorage) Count() int {
	return len(s.fragments)
}

func (s *SetStorage) MissingFragments(total int) []int {
	var missing []int
	for i := 0; i < total; i++ {
		if !s.fragments[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

func (s *SetStorage) OwnedFragments() []int {
	owned := make([]int, 0, len(s.fragments))
	for id := range s.fragments {
		owned = append(owned, id)
	}
	return owned
}

func (s *SetStorage) Clone() StorageStrategy {
	c := NewSetStorage()
	for k, v := range s.fragments {
		c.fragments[k] = v
	}
	return c
}
