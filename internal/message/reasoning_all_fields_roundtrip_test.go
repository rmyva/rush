package message

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReasoningContentAllFieldsRoundTrip(t *testing.T) {
	original := ReasoningContent{}
	populateEveryField(t, reflect.ValueOf(&original).Elem(), "ReasoningContent")

	encoded, err := marshalParts([]ContentPart{original})
	require.NoError(t, err)
	decoded, err := unmarshalParts(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	reasoning, ok := decoded[0].(ReasoningContent)
	require.True(t, ok)
	require.Equal(t, original, reasoning)
}

func populateEveryField(t *testing.T, value reflect.Value, path string) {
	t.Helper()

	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		populateEveryField(t, value.Elem(), path)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			fieldPath := path + "." + value.Type().Field(i).Name
			require.Truef(t, field.CanSet(), "%s cannot be populated", fieldPath)
			populateEveryField(t, field, fieldPath)
		}
	case reflect.String:
		value.SetString(path + " value")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value.SetUint(1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1)
	case reflect.Slice:
		element := reflect.New(value.Type().Elem()).Elem()
		populateEveryField(t, element, path+" element")
		value.Set(reflect.Append(value, element))
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
		key := reflect.New(value.Type().Key()).Elem()
		item := reflect.New(value.Type().Elem()).Elem()
		populateEveryField(t, key, path+" key")
		populateEveryField(t, item, path+" value")
		value.SetMapIndex(key, item)
	default:
		t.Fatalf("unsupported field type at %s: %s", path, value.Type())
	}
}
