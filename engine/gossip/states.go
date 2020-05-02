package gossip

import (
	"encoding/json"

	"github.com/sunmxt/sladder"
)

const (
	// ALIVE State.
	ALIVE = SWIMState(0)
	// SUSPECTED State.
	SUSPECTED = SWIMState(1)
	// DEAD State.
	DEAD = SWIMState(2)
	// LEFT State.
	LEFT = SWIMState(3)
)

// SWIMState stores state of gossip node.
type SWIMState uint8

// SWIMStateNames contains printable name of SWIMState
var SWIMStateNames = map[SWIMState]string{
	ALIVE:     "alive",
	SUSPECTED: "suspected",
	DEAD:      "dead",
	LEFT:      "LEFT",
}

func (s SWIMState) String() string {
	name, exist := SWIMStateNames[s]
	if !exist {
		return "undefined"
	}
	return name
}

// SWIMTag represents node gossip tag.
type SWIMTag struct {
	Version uint32    `json:"v,omitempty"`
	State   SWIMState `json:"s,omitempty"`
	Region  string    `json:"r,omitempty"`
}

// Encode serializes SWIMTags.
func (t *SWIMTag) Encode() string {
	raw, err := json.Marshal(t)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// Decode deserializes SWIMTags.
func (t *SWIMTag) Decode(v string) error {
	return json.Unmarshal([]byte(v), t)
}

// SWIMTagValidator validates SWIMTags.
type SWIMTagValidator struct {
	engine *EngineInstance
}

// Sync synchronizes SWIMTag
func (c *SWIMTagValidator) Sync(entry *sladder.KeyValueEntry, remote *sladder.KeyValue) (bool, error) {
	if remote == nil {
		return false, nil
	}

	remoteTag, localTag := &SWIMTag{}, &SWIMTag{}
	if err := remoteTag.Decode(remote.Value); err != nil { // reject invalid tag.
		c.engine.log.Warn("reject a invalid remote SWIM tag")
		return false, nil
	}

	if err := localTag.Decode(remote.Value); err != nil {
		// invalid local tag. drop it and replace with the remote.
		c.engine.log.Warn("drop invalid local SWIM tag")
		entry.Value = remote.Value
		return true, nil
	}

	// SWIM rule 1: accept newer tag version.
	if remoteTag.Version > localTag.Version {
		entry.Value = remote.Value
		return true, nil
	}
	// SWIM rule 2: reject all older version.
	if remoteTag.Version < localTag.Version {
		return false, nil
	}
	changed := false
	switch {
	case remoteTag.State == SUSPECTED:
		// SWIM rule 3: suspection can be raised by any cluster member, overwriting ALIVE.
		if localTag.State == ALIVE {
			localTag.State, changed = SUSPECTED, true
		}

	case remoteTag.State == DEAD:
		// SWIM rule 4: dead claim overwrites ALIVE, SUSPECTED.
		if localTag.State != LEFT {
			localTag.State, changed = DEAD, true
		}

	case remoteTag.State == LEFT:
		// extended SWIM Rule: LEFT overwrites any.
		localTag.State, changed = LEFT, true
	}

	if changed {
		// update value.
		entry.Value = localTag.Encode()
	}

	return changed, nil
}

// Validate checks whether raw SWIMTags.
func (c *SWIMTagValidator) Validate(kv sladder.KeyValue) bool {
	if kv.Value == "" {
		return true
	}
	tag := &SWIMTagValidator{}
	if err := json.Unmarshal([]byte(kv.Value), tag); err != nil {
		return false
	}
	return true
}

// Txn begins an transaction.
func (c *SWIMTagValidator) Txn(x sladder.KeyValue) (sladder.KVTransaction, error) {
	txn := &SWIMTagTxn{changed: false}
	if err := txn.tag.Decode(x.Value); err != nil {
		return nil, err
	}
	txn.OldVersion = txn.tag.Version
	return txn, nil
}

// SWIMTagTxn implements SWIM tag transaction.
type SWIMTagTxn struct {
	tag        SWIMTag
	changed    bool
	OldVersion uint32
}

// After returns modified value.
func (t *SWIMTagTxn) After() (bool, string) { return t.changed, t.tag.Encode() }

// Region returns region of tag snapshot
func (t *SWIMTagTxn) Region() string { return t.tag.Region }

// State returns current SWIM state.
func (t *SWIMTagTxn) State() SWIMState { return t.tag.State }

// ClaimDead set SWIM state to dead.
func (t *SWIMTagTxn) ClaimDead() bool {
	if t.tag.State == LEFT {
		// extended SWIM Rule: LEFT overwrites any.
		return false
	}
	if t.tag.State != DEAD {
		// SWIM Rule: DEAD overwrites SUSPECTED and ALIVE.
		t.changed = true
		t.tag.State = DEAD
		return true
	}
	return false
}

// ClaimSuspected set SWIM state to SUSPECTED.
func (t *SWIMTagTxn) ClaimSuspected() bool {
	if t.tag.State == DEAD {
		// SWIM Rule: DEAD overwrites SUSPECTED.
		return false
	}
	if t.tag.State == LEFT {
		// extended SWIM Rule: LEFT overwrites any.
		return false
	}
	if t.tag.State == SUSPECTED {
		return false
	}
	t.tag.State, t.changed = SUSPECTED, true
	return true
}

// ClaimAlive clears false positive and ensure SWIM state is ALIVE.
func (t *SWIMTagTxn) ClaimAlive() bool {
	if t.tag.State != ALIVE {
		// clear false positive by raising version.
		if t.tag.Version < t.OldVersion {
			t.tag.Version++
		}
		t.tag.State, t.changed = ALIVE, true
		return true
	}
	return false
}

// Leave set states to LEFT.
func (t *SWIMTagTxn) Leave() bool {
	if t.tag.State == LEFT {
		return true
	}
	t.tag.State, t.changed = LEFT, true
	return true
}

// SetRegion updates region.
func (t *SWIMTagTxn) SetRegion(region string) string {
	old := t.tag.Region
	if old != region {
		t.tag.Region, t.changed = region, true
	}
	return old
}

func (e *EngineInstance) onSelfSWIMTagMissing(self *sladder.Node) {
	self.Keys(e.swimTagKey).Txn(func(swim *SWIMTagTxn) (bool, error) {
		swim.SetRegion(e.Region)
		swim.ClaimAlive()
		return true, nil
	})
}

func (e *EngineInstance) onSelfSWIMStateChanged(self *sladder.Node, old, new *SWIMTag) {
	if new.State != ALIVE { // clear false postive.
		self.Keys(e.swimTagKey).Txn(func(swim *SWIMTagTxn) (bool, error) {
			return swim.ClaimAlive(), nil
		})
	}
}
