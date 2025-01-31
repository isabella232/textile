package core

import (
	"context"
	"errors"

	ma "github.com/multiformats/go-multiaddr"
	dbpb "github.com/textileio/go-threads/api/pb"
	"github.com/textileio/go-threads/core/thread"
	netpb "github.com/textileio/go-threads/net/api/pb"
	"github.com/textileio/textile/v2/api/common"
	mdb "github.com/textileio/textile/v2/mongodb"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// threadInterceptor monitors for thread creation and deletion.
// Textile tracks threads against dev, org, and user accounts.
// Users must supply a valid API key from a dev/org.
func (t *Textile) threadInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		method, _ := grpc.Method(ctx)
		for _, ignored := range authIgnoredMethods {
			if method == ignored {
				return handler(ctx, req)
			}
		}
		for _, block := range blockMethods {
			if method == block {
				return nil, status.Error(codes.PermissionDenied, "Method is not accessible")
			}
		}
		if sid, ok := common.SessionFromContext(ctx); ok && sid == t.internalHubSession {
			return handler(ctx, req)
		}

		account, ok := mdb.AccountFromContext(ctx)
		if !ok {
			// Should not happen at this point in the interceptor chain
			return nil, status.Errorf(codes.FailedPrecondition, "account is required")
		}

		var newID thread.ID
		var isDB bool
		var err error
		switch method {
		case "/threads.pb.API/NewDB":
			newID, err = thread.Cast(req.(*dbpb.NewDBRequest).DbID)
			if err != nil {
				return nil, err
			}
			isDB = true
		case "/threads.pb.API/NewDBFromAddr":
			addr, err := ma.NewMultiaddrBytes(req.(*dbpb.NewDBFromAddrRequest).Addr)
			if err != nil {
				return nil, err
			}
			newID, err = thread.FromAddr(addr)
			if err != nil {
				return nil, err
			}
			isDB = true
		case "/threads.net.pb.API/CreateThread":
			newID, err = thread.Cast(req.(*netpb.CreateThreadRequest).ThreadID)
			if err != nil {
				return nil, err
			}
		case "/threads.net.pb.API/AddThread":
			addr, err := ma.NewMultiaddrBytes(req.(*netpb.AddThreadRequest).Addr)
			if err != nil {
				return nil, err
			}
			newID, err = thread.FromAddr(addr)
			if err != nil {
				return nil, err
			}
		default:
			// If we're dealing with an existing thread, make sure that the owner
			// owns the thread directly or via an API key.
			// Hub APIs that deal with threads require the id to be passed in the context.
			// go-threads APIs take the ID as a request param, which means we have to
			// extract it from the request.
			threadIDFromCtx, _ := common.ThreadIDFromContext(ctx)
			threadIDFromReq, err := getThreadIDFromRequest(method, req)
			if err != nil {
				return nil, err
			}
			ids := []thread.ID{threadIDFromCtx, threadIDFromReq}
			if err := t.checkThreadOwner(ctx, method, ids, account); err != nil {
				return nil, err
			}
		}

		// Preemptively track the new thread ID for the owner.
		// This needs to happen before the request is handled in case there's a conflict
		// with the owner and thread name.
		if newID.Defined() {
			thds, err := t.collections.Threads.ListByOwner(ctx, account.Owner().Key)
			if err != nil {
				return nil, err
			}
			if t.conf.MaxNumberThreadsPerOwner > 0 && len(thds) >= t.conf.MaxNumberThreadsPerOwner {
				return nil, ErrTooManyThreadsPerOwner
			}
			if _, err := t.collections.Threads.Create(ctx, newID, account.Owner().Key, isDB); err != nil {
				return nil, err
			}
		}

		// Track the thread ID marked for deletion.
		var deleteID thread.ID
		switch method {
		case "/threads.pb.API/DeleteDB":
			deleteID, err = thread.Cast(req.(*dbpb.DeleteDBRequest).DbID)
			if err != nil {
				return nil, err
			}
			keys, err := t.collections.IPNSKeys.ListByThreadID(ctx, deleteID)
			if err != nil {
				return nil, err
			}
			if len(keys) != 0 {
				return nil, status.Error(codes.FailedPrecondition, "DB not empty (delete buckets first)")
			}
		case "/threads.net.pb.API/DeleteThread":
			deleteID, err = thread.Cast(req.(*netpb.DeleteThreadRequest).ThreadID)
			if err != nil {
				return nil, err
			}
		}

		// Let the request pass through.
		res, err := handler(ctx, req)
		if err != nil {
			// Clean up the new thread if there was an error.
			if newID.Defined() {
				if err := t.collections.Threads.Delete(ctx, newID, account.Owner().Key); err != nil {
					log.Errorf("error deleting thread %s: %v", newID, err)
				}
			}
			return res, err
		}

		// Clean up the tracked thread if it was deleted.
		if deleteID.Defined() {
			if err := t.collections.Threads.Delete(ctx, deleteID, account.Owner().Key); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
}

func (t *Textile) checkThreadOwner(
	ctx context.Context,
	method string,
	threadIDs []thread.ID,
	account *mdb.AccountCtx,
) error {
	for _, id := range threadIDs {
		if !id.Defined() {
			continue
		}
		th, err := t.collections.Threads.Get(ctx, id, account.Owner().Key)
		if err != nil && errors.Is(err, mongo.ErrNoDocuments) {
			// Allow non-owners to interact with a limited set of APIs.
			var isAllowed bool
			for _, m := range allowedCrossUserMethods {
				if method == m {
					isAllowed = true
					break
				}
			}
			if !isAllowed {
				return status.Error(codes.PermissionDenied, "User does not own thread")
			}
		} else if err != nil {
			return err
		}
		if th != nil {
			key, _ := mdb.APIKeyFromContext(ctx)
			if key != nil && key.Type == mdb.UserKey {
				// Extra user check for user API keys.
				if key.Key != th.Key {
					return status.Error(codes.PermissionDenied, "Bad API key")
				}
			}
		}
	}
	return nil
}

func getThreadIDFromRequest(method string, req interface{}) (thread.ID, error) {
	var id thread.ID
	var err error
	switch method {
	case "/threads.pb.API/GetDBInfo":
		id, err = thread.Cast(req.(*dbpb.GetDBInfoRequest).DbID)
	case "/threads.pb.API/DeleteDB":
		id, err = thread.Cast(req.(*dbpb.DeleteDBRequest).DbID)
	case "/threads.pb.API/NewCollection":
		id, err = thread.Cast(req.(*dbpb.NewCollectionRequest).DbID)
	case "/threads.pb.API/UpdateCollection":
		id, err = thread.Cast(req.(*dbpb.UpdateCollectionRequest).DbID)
	case "/threads.pb.API/DeleteCollection":
		id, err = thread.Cast(req.(*dbpb.DeleteCollectionRequest).DbID)
	case "/threads.pb.API/GetCollectionInfo":
		id, err = thread.Cast(req.(*dbpb.GetCollectionInfoRequest).DbID)
	case "/threads.pb.API/GetCollectionIndexes":
		id, err = thread.Cast(req.(*dbpb.GetCollectionIndexesRequest).DbID)
	case "/threads.pb.API/ListCollections":
		id, err = thread.Cast(req.(*dbpb.ListCollectionsRequest).DbID)
	case "/threads.net.pb.API/GetThread":
		id, err = thread.Cast(req.(*netpb.GetThreadRequest).ThreadID)
	case "/threads.net.pb.API/PullThread":
		id, err = thread.Cast(req.(*netpb.PullThreadRequest).ThreadID)
	case "/threads.net.pb.API/DeleteThread":
		id, err = thread.Cast(req.(*netpb.DeleteThreadRequest).ThreadID)
	case "/threads.net.pb.API/AddReplicator":
		id, err = thread.Cast(req.(*netpb.AddReplicatorRequest).ThreadID)
	case "/threads.net.pb.API/CreateRecord":
		id, err = thread.Cast(req.(*netpb.CreateRecordRequest).ThreadID)
	case "/threads.net.pb.API/AddRecord":
		id, err = thread.Cast(req.(*netpb.AddRecordRequest).ThreadID)
	case "/threads.net.pb.API/GetRecord":
		id, err = thread.Cast(req.(*netpb.GetRecordRequest).ThreadID)
	}
	return id, err
}
