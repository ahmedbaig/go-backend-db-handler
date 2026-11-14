package docs

import (
	"context"
	"fmt"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	EventsCollection = "events"
	DAUCollection    = "daily_active_users"
	maxFuture        = 5 * time.Minute
)

func (p *Pool) Track(ctx context.Context, storeName string, evs []db.Event, requestID string, now time.Time) (db.TrackResult, error) {
	s, err := p.get(storeName)
	if err != nil {
		return db.TrackResult{}, err
	}
	if len(evs) == 0 {
		return db.TrackResult{}, fieldErr("events", "required", "is required")
	}
	now = now.UTC()
	docs := make([]any, len(evs))
	type dauKey struct{ day, user string }
	dau := map[dauKey]struct {
		first, last time.Time
		n           int
	}{}
	for i, e := range evs {
		at := e.At.UTC()
		if at.IsZero() || at.After(now.Add(maxFuture)) {
			at = now
		}
		d := bson.D{
			{Key: "name", Value: e.Name},
			{Key: "at", Value: at},
			{Key: "receivedAt", Value: now},
		}
		if e.UserID != "" {
			d = append(d, bson.E{Key: "userId", Value: e.UserID})
		}
		if e.SessionID != "" {
			d = append(d, bson.E{Key: "sessionId", Value: e.SessionID})
		}
		if len(e.Props) > 0 {
			props, err := bson.Marshal(e.Props)
			if err != nil {
				return db.TrackResult{}, fieldErr(fmt.Sprintf("events.%d.props", i), "type", "must be a JSON object")
			}
			var pd bson.D
			_ = bson.Unmarshal(props, &pd)
			if err := check(fmt.Sprintf("events.%d.props", i), pd, 0); err != nil {
				return db.TrackResult{}, err
			}
			d = append(d, bson.E{Key: "props", Value: pd})
		}
		if requestID != "" {
			d = append(d, bson.E{Key: "requestId", Value: requestID})
		}
		docs[i] = d
		if e.UserID != "" {
			k := dauKey{day: at.Format(time.DateOnly), user: e.UserID}
			cur, ok := dau[k]
			if !ok || at.Before(cur.first) {
				cur.first = at
			}
			if at.After(cur.last) {
				cur.last = at
			}
			cur.n++
			dau[k] = cur
		}
	}
	if _, err := s.db.Collection(EventsCollection).InsertMany(ctx, docs); err != nil {
		return db.TrackResult{}, translate(err)
	}
	if len(dau) > 0 {
		models := make([]mongo.WriteModel, 0, len(dau))
		for k, v := range dau {
			models = append(models, mongo.NewUpdateOneModel().
				SetFilter(bson.D{{Key: "_id", Value: k.day + ":" + k.user}}).
				SetUpdate(bson.D{
					{Key: "$setOnInsert", Value: bson.D{{Key: "day", Value: k.day}, {Key: "userId", Value: k.user}}},
					{Key: "$min", Value: bson.D{{Key: "first", Value: v.first}}},
					{Key: "$max", Value: bson.D{{Key: "last", Value: v.last}}},
					{Key: "$inc", Value: bson.D{{Key: "events", Value: v.n}}},
				}).
				SetUpsert(true))
		}
		if _, err := s.db.Collection(DAUCollection).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
			return db.TrackResult{}, translate(err)
		}
	}
	return db.TrackResult{Accepted: len(evs)}, nil
}

func (p *Pool) DAU(ctx context.Context, storeName, from, to string) ([]db.DAUPoint, error) {
	s, err := p.get(storeName)
	if err != nil {
		return nil, err
	}
	match := bson.D{}
	rng := bson.D{}
	if from != "" {
		rng = append(rng, bson.E{Key: "$gte", Value: from})
	}
	if to != "" {
		rng = append(rng, bson.E{Key: "$lte", Value: to})
	}
	if len(rng) > 0 {
		match = append(match, bson.E{Key: "day", Value: rng})
	}
	cur, err := s.db.Collection(DAUCollection).Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$day"}, {Key: "users", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
		bson.D{{Key: "$limit", Value: 3660}},
	})
	if err != nil {
		return nil, translate(err)
	}
	var rows []struct {
		Day   string `bson:"_id"`
		Users int64  `bson:"users"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, translate(err)
	}
	out := make([]db.DAUPoint, len(rows))
	for i, r := range rows {
		out[i] = db.DAUPoint{Day: r.Day, Users: r.Users}
	}
	return out, nil
}

func (p *Pool) EnsureIndexes(ctx context.Context) error {
	for _, s := range p.stores {
		_, err := s.db.Collection(EventsCollection).Indexes().CreateMany(ctx, []mongo.IndexModel{
			{Keys: bson.D{{Key: "name", Value: 1}, {Key: "at", Value: -1}}},
			{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "at", Value: -1}}},
		})
		if err != nil {
			return err
		}
		if _, err := s.db.Collection(DAUCollection).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "day", Value: 1}}}); err != nil {
			return err
		}
	}
	return nil
}
