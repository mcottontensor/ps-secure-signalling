package protocol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The protocol's schema is Common/protobuf/signalling_messages.proto in PixelStreamingInfrastructure.
// testdata holds a copy (upstream a672d2fe, 2025-02-04). To check against a newer one:
//
//	PS_SIGNALLING_PROTO=../PixelStreamingInfrastructure/Common/protobuf/signalling_messages.proto go test ./pkg/protocol
//
// The proto is the contract, not a code generator: messages travel as JSON, and the Go types are
// written by hand so they can accept what peers really send (see the compatible types below).
func loadSchema(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	path := os.Getenv("PS_SIGNALLING_PROTO")
	if path == "" {
		path = filepath.Join("testdata", "signalling_messages.proto")
	}
	c := protocompile.Compiler{Resolver: &protocompile.SourceResolver{ImportPaths: []string{filepath.Dir(path)}}}
	files, err := c.Compile(context.Background(), filepath.Base(path))
	if err != nil {
		t.Fatalf("compiling %s: %v", path, err)
	}
	return files[0]
}

// Messages in the proto that are not wire messages of their own.
var schemaOnly = map[string]bool{
	"base_message":          true, // the shared "type" field, our Header
	"peerConnectionOptions": true, // an arbitrary RTCConfiguration, carried as raw JSON
}

// nested maps proto messages used only as field types to their Go structs.
var nested = map[string]reflect.Type{
	"iceCandidateData": reflect.TypeOf(IceCandidateData{}),
}

// knownGaps are fields that real peers send but the schema does not declare. Remove an entry
// once the upstream proto declares the field.
var knownGaps = map[string]string{
	// The signalling server stamps messages it relays from a player with that player's id (the
	// reference Wilbur does too, in PlayerConnection.sendToStreamer).
	"DataChannelRequest.playerId":    "stamped by the signalling server",
	"PeerDataChannelsReady.playerId": "stamped by the signalling server",
	// The SFU names the player the channels are for; PS1 refuses the message without it
	// (FPixelStreamingSignallingConnection::OnSFUPeerDataChannels).
	"StreamerDataChannels.playerId": "set by the SFU, required by PS1",
}

var (
	tString   = reflect.TypeOf("")
	tPlayerID = reflect.TypeOf(PlayerID(""))
	tBool     = reflect.TypeOf(false)
	tInt      = reflect.TypeOf(Int(0))
	tNumber   = reflect.TypeOf(Number(""))
	tRaw      = reflect.TypeOf(json.RawMessage(nil))
)

// compatible reports whether a Go field type can carry a proto field. Some are deliberately wider
// than the schema, because real peers send more than it allows:
//   - string fields may be PlayerID, which also accepts the numbers PS1 sends
//   - int32 fields may be Number, because browsers send millisecond timestamps in ping
func compatible(f protoreflect.FieldDescriptor, goType reflect.Type) bool {
	if goType.Kind() == reflect.Pointer {
		goType = goType.Elem() // optional fields
	}
	if f.IsList() {
		if goType.Kind() != reflect.Slice || f.Kind() != protoreflect.StringKind {
			return false
		}
		return goType.Elem() == tString
	}
	switch f.Kind() {
	case protoreflect.StringKind:
		return goType == tString || goType == tPlayerID
	case protoreflect.BoolKind:
		return goType == tBool
	case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return goType == tInt || goType == tNumber
	case protoreflect.MessageKind:
		name := string(f.Message().Name())
		if schemaOnly[name] {
			return goType == tRaw
		}
		return nested[name] == goType
	}
	return false
}

// jsonFields lists a struct's fields by JSON name, skipping the embedded Header.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = f.Type
	}
	return out
}

// checkFields compares one proto message with one Go struct, in both directions.
func checkFields(t *testing.T, msg protoreflect.MessageDescriptor, goType reflect.Type) {
	t.Helper()
	goFields := jsonFields(goType)
	fields := msg.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		name := string(f.Name()) // the wire uses proto field names verbatim, not proto3 JSON names
		if name == "type" {
			continue
		}
		gt, ok := goFields[name]
		switch {
		case !ok:
			t.Errorf("%s.%s is in the schema but not in %s", msg.Name(), name, goType.Name())
		case !compatible(f, gt):
			t.Errorf("%s.%s is %v in the schema but %v in %s", msg.Name(), name, f.Kind(), gt, goType.Name())
		}
		delete(goFields, name)
	}
	for name := range goFields {
		if _, gap := knownGaps[goType.Name()+"."+name]; !gap {
			t.Errorf("%s.%s is not in the schema", goType.Name(), name)
		}
	}
}

// TestMatchesSchema keeps the Go types and the proto in step: every proto message is a registered
// type with the same fields, and every registered type is in the proto.
func TestMatchesSchema(t *testing.T) {
	schema := loadSchema(t)
	seen := map[string]bool{}
	msgs := schema.Messages()
	for i := range msgs.Len() {
		msg := msgs.Get(i)
		name := string(msg.Name())
		if schemaOnly[name] {
			continue
		}
		if goType, ok := nested[name]; ok {
			checkFields(t, msg, goType)
			continue
		}
		ctor, ok := registry[name]
		if !ok {
			t.Errorf("message %q is in the schema but not registered", name)
			continue
		}
		seen[name] = true
		if msg.Fields().ByName("type") == nil {
			t.Errorf("message %q has no type field", name)
		}
		checkFields(t, msg, reflect.TypeOf(ctor()).Elem())
	}
	for name := range registry {
		if !seen[name] {
			t.Errorf("registered message %q is not in the schema", name)
		}
	}
}
