package melta

import (
	"context"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestMissingMetadataProducersReturnPublicNotFoundClass proves the public
// class comes from the producer boundary, not the HTTP mapper: every real
// unknown-provider and unknown-model producer refusal satisfies the public
// harness.ErrNotFound identity directly, so the transport only maps that
// public sentinel to the wire class.
func TestMissingMetadataProducersReturnPublicNotFoundClass(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		caller := testCaller(r)
		for name, run := range map[string]func() error{
			"provider read": func() error {
				_, err := r.GetProvider(ctx, caller, "ghost")
				return err
			},
			"provider delete": func() error {
				_, err := r.DeleteProvider(ctx, caller, "ghost")
				return err
			},
			"provider field reset": func() error {
				_, err := r.ResetProviderField(ctx, caller, "ghost", protocol.ProviderFieldName)
				return err
			},
			"model delete": func() error {
				_, err := r.DeleteModel(ctx, caller, "prov", "ghost")
				return err
			},
			"model field reset": func() error {
				_, err := r.ResetModelField(ctx, caller, "prov", "ghost", protocol.ModelFieldName)
				return err
			},
			"model delete under missing provider": func() error {
				_, err := r.DeleteModel(ctx, caller, "ghost", "m")
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				err := run()
				testassert.Require(t, err != nil && errors.Is(err, harness.ErrNotFound), "producer refusal = %v, want the public not-found class", err)
				testassert.Require(t, ErrorCode(err) == protocol.ErrorCodeNotFound, "classify(%v) = %s, want not_found", err, ErrorCode(err))
			})
		}
	})
}

// TestClassifyProtocolMissingMetadata proves the producer's typed identity and
// the public class mapping for the missing-metadata, invalid-input, and
// complete-candidate refusal siblings.
func TestClassifyProtocolMissingMetadata(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		caller := testCaller(r)
		reserved := map[string]any{"stream": true}

		rows := []struct {
			name string
			run  func() error
			want error
			code protocol.ErrorCode
		}{
			{"provider read missing", func() error {
				_, err := r.GetProvider(ctx, caller, "ghost")
				return err
			}, catalog.ErrUnknownProvider, protocol.ErrorCodeNotFound},
			{"provider delete missing", func() error {
				_, err := r.DeleteProvider(ctx, caller, "ghost")
				return err
			}, catalog.ErrUnknownProvider, protocol.ErrorCodeNotFound},
			{"provider field reset missing", func() error {
				_, err := r.ResetProviderField(ctx, caller, "ghost", protocol.ProviderFieldName)
				return err
			}, catalog.ErrUnknownProvider, protocol.ErrorCodeNotFound},
			{"model delete missing", func() error {
				_, err := r.DeleteModel(ctx, caller, "prov", "ghost")
				return err
			}, catalog.ErrUnknownModel, protocol.ErrorCodeNotFound},
			{"model field reset missing", func() error {
				_, err := r.ResetModelField(ctx, caller, "prov", "ghost", protocol.ModelFieldName)
				return err
			}, catalog.ErrUnknownModel, protocol.ErrorCodeNotFound},
			{"model delete under missing provider", func() error {
				_, err := r.DeleteModel(ctx, caller, "ghost", "m")
				return err
			}, catalog.ErrUnknownProvider, protocol.ErrorCodeNotFound},
			{"empty provider identity", func() error {
				_, err := r.DeleteProvider(ctx, caller, "")
				return err
			}, harness.ErrInvalid, protocol.ErrorCodeInvalid},
			{"empty model identity", func() error {
				_, err := r.DeleteModel(ctx, caller, "prov", "")
				return err
			}, harness.ErrInvalid, protocol.ErrorCodeInvalid},
			{"invalid model field on a missing model", func() error {
				_, err := r.ResetModelField(ctx, caller, "prov", "ghost", protocol.ModelField("bogus"))
				return err
			}, harness.ErrInvalid, protocol.ErrorCodeInvalid},
			{"complete candidate refusal", func() error {
				_, err := r.SaveModel(ctx, caller, "prov", "m", protocol.ModelEdit{ExtraBody: &reserved})
				return err
			}, ErrConfiguration, protocol.ErrorCodeConfiguration},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				err := row.run()
				testassert.Require(t, err != nil, "the producer refused nothing, want a typed refusal")
				testassert.Require(t, errors.Is(err, row.want), "producer refusal = %v, want class %v", err, row.want)
				testassert.Require(t, ErrorCode(err) == row.code, "classify(%v) = %s, want %s", err, ErrorCode(err), row.code)
			})
		}
	})
}
