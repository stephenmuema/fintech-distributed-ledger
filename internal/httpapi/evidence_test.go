package httpapi_test

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
)

// The MongoDB evidence copy, against a real MongoDB (ZIRNIS_TEST_MONGODB_URI, e.g. the CI
// service container). The relational store stays the system of record; this proves the copy is
// complete, keyed and idempotent.
func TestEvidenceIsCopiedToMongoDB(t *testing.T) {
	uri := os.Getenv("ZIRNIS_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("ZIRNIS_TEST_MONGODB_URI not set")
	}
	database := "zr_" + domain.RandomHex(6)
	h := newHarness(t, func(c *config.Config) { c.MongoURI, c.MongoDatabase = uri, database })
	h.fundedAccount(50_000, "Business") // customers, KYC, account, a card payment: audit events + provider calls

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	audits := h.amina().get("/ops/audit/verify").expect(200).num("checkedEvents")

	count := func(collection string) int64 {
		n, err := client.Database(database).Collection(collection).CountDocuments(context.Background(), bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(20 * time.Second)
	for count("audit_events") < audits && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	eq(t, audits, count("audit_events"))
	check(t, count("provider_calls") >= 1, "provider calls were not copied")

	// Catch-up after the fact is idempotent: documents are upserted by key, never duplicated.
	n, err := h.sys.Evidence.CatchUp(context.Background(), h.sys.Store)
	check(t, err == nil, err)
	eq(t, 0, n)
	eq(t, audits, count("audit_events"))

	var first bson.M
	err = client.Database(database).Collection("audit_events").FindOne(context.Background(), bson.M{"_id": int64(1)}).Decode(&first)
	check(t, err == nil, err)
	check(t, first["hash"] != nil && first["previous_hash"] == domain.GenesisHash, "the copy must carry the hash chain: ", first)
}
