package storage

import (
	"bytes"
	"sort"
)

const (
	LevelRoot    = 0x0001
	LevelYear    = 0x0002
	LevelMonth   = 0x0004
	LevelDay     = 0x0008
	LevelHour    = 0x0010
	LevelMinute  = 0x0020
	LevelSecond  = 0x0040
	LevelMSecond = 0x0080
	LevelUSecond = 0x0100
	LevelNSecond = 0x0200

	LevelFlag         = 0x0FFF
	LeafFlag          = 0x3000
	InteriorChunkFlag = 0x1000
	LeafChunkFlag     = 0x2000
)

// node represents an in-memory, deserialized page.
type node struct {
	db     *DB
	level  uint16
	key    int64
	parent *node
	isLeaf bool

	dirty    int            // the dirty node will be flushed
	pointers []*nodePointer // interior nodes will have this
	points   []*Point       // leaf nodes will have this
}

type Value struct {
	sum   float64
	max   float64
	min   float64
	first float64
	last  float64
	count uint16
}

type nodePointer struct {
	key     int64
	pos     int64
	pointer *node
	value   map[string]Value
}

func (v *Value) encode() []byte {
	buf := new(bytes.Buffer)
	buf.Write(encodeFloat64(v.sum))
	buf.Write(encodeFloat64(v.max))
	buf.Write(encodeFloat64(v.min))
	buf.Write(encodeFloat64(v.first))
	buf.Write(encodeFloat64(v.last))
	buf.Write(encodeUint16(v.count))
	return buf.Bytes()
}

func decodeValue(valueBytes []byte) Value {
	v := Value{}
	v.sum = decodeFloat64(valueBytes[0:8])
	v.max = decodeFloat64(valueBytes[8:16])
	v.min = decodeFloat64(valueBytes[16:24])
	v.first = decodeFloat64(valueBytes[24:32])
	v.last = decodeFloat64(valueBytes[32:40])
	v.count = decodeUint16(valueBytes[40:42])
	return v
}

func (np *nodePointer) encode() []byte {
	buf := new(bytes.Buffer)
	buf.Write(encodeInt64(np.key))
	buf.Write(encodeInt64(np.pos))
	for k, v := range np.value {
		keyBytes := []byte(k)
		buf.Write(encodeUint16(uint16(len(keyBytes))))
		buf.Write(keyBytes)
		buf.Write(v.encode())
	}
	return buf.Bytes()
}

func decodeNodePointer(npBytes []byte) (*nodePointer, error) {
	np := &nodePointer{}
	np.key = decodeInt64(npBytes[0:8])
	np.pos = decodeInt64(npBytes[8:16])

	np.value = make(map[string]Value)
	bufPos := 16
	for bufPos < len(npBytes) {
		keyLength := int(decodeUint16(npBytes[bufPos : bufPos+2]))
		bufPos += 2
		key := string(npBytes[bufPos : bufPos+keyLength])
		bufPos += keyLength
		value := decodeValue(npBytes[bufPos : bufPos+42])
		bufPos += 42
		np.value[key] = value
	}

	return np, nil
}

func (n *node) encode() []byte {
	buf := new(bytes.Buffer)
	if n.isLeaf {
		buf.Write(encodeUint16(n.level | LeafChunkFlag))
		for _, point := range n.points {
			pointBytes := point.encode()
			buf.Write(encodeUint16(uint16(len(pointBytes))))
			buf.Write(pointBytes)
		}
	} else {
		buf.Write(encodeUint16(n.level | InteriorChunkFlag))
		for _, pointer := range n.pointers {
			pointerBytes := pointer.encode()
			buf.Write(encodeUint16(uint16(len(pointerBytes))))
			buf.Write(pointerBytes)
		}
	}
	return buf.Bytes()
}

func (db *DB) decodeNode(nodeBytes []byte) (*node, error) {
	flags := decodeUint16(nodeBytes[0:2])
	if flags&LeafFlag == LeafChunkFlag {
		return db.decodeLeafNode(nodeBytes)
	}
	return db.decodeInteriorNode(nodeBytes)
}

func (db *DB) decodeLeafNode(nodeBytes []byte) (*node, error) {
	n := db.newLeafNode()
	n.level = decodeUint16(nodeBytes[0:2]) & LevelFlag

	bufPos := 2
	for bufPos < len(nodeBytes) {
		pointLength := int(decodeUint16(nodeBytes[bufPos : bufPos+2]))
		bufPos += 2
		point, err := decodePoint(nodeBytes[bufPos : bufPos+pointLength])
		if err != nil {
			return nil, err
		}
		bufPos += pointLength
		n.points = append(n.points, point)
	}
	return n, nil
}

func (db *DB) decodeInteriorNode(nodeBytes []byte) (*node, error) {
	n := db.newInteriorNode()
	n.level = decodeUint16(nodeBytes[0:2]) & LevelFlag

	bufPos := 2
	for bufPos < len(nodeBytes) {
		pointerLength := int(decodeUint16(nodeBytes[bufPos : bufPos+2]))
		bufPos += 2
		pointer, err := decodeNodePointer(nodeBytes[bufPos : bufPos+pointerLength])
		if err != nil {
			return nil, err
		}
		bufPos += pointerLength
		n.pointers = append(n.pointers, pointer)
	}
	return n, nil
}

func (db *DB) newLeafNode() *node {
	return &node{
		db:     db,
		isLeaf: true,
		points: make([]*Point, 0),
	}
}

func (db *DB) newInteriorNode() *node {
	return &node{
		db:       db,
		isLeaf:   false,
		dirty:    -1,
		pointers: make([]*nodePointer, 0),
	}
}

// flush node to disk.
func (n *node) flush() int64 {
	flags := n.level
	if !n.isLeaf {
		n.pointers[n.dirty].pos = n.pointers[n.dirty].pointer.flush()
		flags = flags | InteriorChunkFlag
	} else {
		flags = flags | LeafChunkFlag
	}
	nodeBytes := n.encode()

	pos, _, err := n.db.writeChunk(nodeBytes)
	if err != nil {
		return int64(0)
	}

	return pos
}

func (n *node) put(t *Time, value map[string]float64) error {
	var err error
	if n.isLeaf {
		err = n.insertPoint(t, value)
	} else {
		err = n.insertNode(t, value)
	}
	if err != nil {
		return err
	}

	n.db.root.reduce()
	return nil
}

func (n *node) insertPoint(t *Time, value map[string]float64) error {
	index := sort.Search(len(n.points), func(i int) bool {
		return n.points[i].Timestamp >= t.TS
	})
	if n.points[index].Timestamp == t.TS {
		n.points[index].Value = value
	} else {
		n.points = append(n.points, &Point{})
		copy(n.points[index+1:], n.points[index:])

		n.points[index] = &Point{Timestamp: t.TS, Value: value}
	}

	return nil
}

func (n *node) insertNode(t *Time, value map[string]float64) error {
	_assert(t.Level()>>2 >= n.level, "insertNode must bigger than parent of point")
	if t.Level()>>2 == n.level {
		leafNode := n.db.newLeafNode()
		leafNode.parent = n
		leafNode.level = n.level << 1

		point := Point{
			Timestamp: t.TS,
			Value:     value,
		}
		leafNode.points = append(leafNode.points, &point)

		index := sort.Search(len(n.pointers), func(i int) bool {
			return n.pointers[i].key >= t.TS
		})
		if len(n.pointers) == 0 || index >= len(n.pointers) || n.pointers[index].key != t.TS {
			n.pointers = append(n.pointers, &nodePointer{})
			copy(n.pointers[index+1:], n.pointers[index:])
		}

		np := nodePointer{
			key:     t.Timestamp(n.level << 1).UnixNano(),
			pointer: leafNode,
		}
		n.pointers[index] = &np
		n.dirty = index

		return nil
	}

	interiorNode := n.db.newInteriorNode()
	interiorNode.level = n.level << 1
	interiorNode.parent = n

	interiorNode.insertNode(t, value)

	index := sort.Search(len(n.pointers), func(i int) bool {
		return n.pointers[i].key >= t.TS
	})
	if len(n.pointers) == 0 || index >= len(n.pointers) || n.pointers[index].key != t.TS {
		n.pointers = append(n.pointers, &nodePointer{})
		copy(n.pointers[index+1:], n.pointers[index:])
	}

	np := nodePointer{
		key:     t.Timestamp(n.level << 1).UnixNano(),
		pointer: interiorNode,
	}
	n.pointers[index] = &np
	n.dirty = index

	return nil
}

// expand leafnode to iterior node
func (n *node) expand() {
	n.isLeaf = false

	for _, point := range n.points {
		leafNode := n.db.newLeafNode()
		leafNode.level = n.level << 1
		leafNode.points = append(leafNode.points, point)

		np := nodePointer{
			key:     point.Timestamp,
			pos:     leafNode.flush(),
			pointer: leafNode,
		}
		n.pointers = append(n.pointers, &np)
	}
}

func (n *node) reduce() map[string]Value {
	value := make(map[string]Value)
	if n.isLeaf {
		for index, point := range n.points {
			for k, v := range point.Value {
				if vk, ok := value[k]; !ok {
					value[k] = Value{
						sum:   v,
						max:   v,
						min:   v,
						first: v,
						last:  v,
						count: 1,
					}
				} else {
					vk.sum += v
					if vk.max < v {
						vk.max = v
					} else if value[k].min > v {
						vk.min = v
					}
					if index == len(n.points)-1 {
						vk.last = v
					}
					vk.count++

					value[k] = vk
				}
			}
		}
	} else {
		n.pointers[n.dirty].value = n.pointers[n.dirty].pointer.reduce()
		for index, pointer := range n.pointers {
			for k, v := range pointer.value {
				if vk, ok := value[k]; !ok {
					value[k] = v
				} else {
					vk.sum += v.sum
					if vk.max < v.max {
						vk.max = v.max
					}
					if value[k].min < v.min {
						vk.min = v.min
					}
					if index == len(n.pointers)-1 {
						vk.last = v.last
					}
					vk.count += v.count
					value[k] = vk
				}
			}
		}
	}
	return value
}
