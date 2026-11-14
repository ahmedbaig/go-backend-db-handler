# go-backend-db-handler/types

Generated Go types for every table go-backend-db-handler serves. Import this module from a service instead of re-declaring table structs: it has no GORM or driver dependencies, only [go-backend-db](https://github.com/ahmedbaig/go-backend-db).

```sh
go get github.com/ahmedbaig/go-backend-db-handler/types@latest
```

```go
import "github.com/ahmedbaig/go-backend-db-handler/types/demo"

posts := demo.PostService(dbClient)
list, err := posts.FindMany(ctx, db.Params{
	Where:   db.W().Eq(demo.PostFields.Published, true),
	Include: []string{demo.PostRelations.Author},
	Take:    db.Take(20),
})
```

Per table, in package `<database>`:

| Generated | Example |
|---|---|
| struct with JSON tags | `demo.Post` |
| table name | `demo.PostTable` |
| filterable field names | `demo.PostFields.AuthorID` → `"authorId"` |
| relation names for `Include` | `demo.PostRelations.Author` |
| typed service | `demo.PostService(client)` |
| logical database name | `demo.Database` |

Do not edit: run `make gen` (introspect databases) or `go run ./cmd/db-gen -types-only` (from existing models) in the handler repo, then tag `types/vX.Y.Z`.
