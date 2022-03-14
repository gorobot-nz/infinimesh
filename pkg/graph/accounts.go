/*
Copyright © 2021-2022 Infinite Devices GmbH, Nikita Ivanovski info@slnt-opp.xyz

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package graph

import (
	"context"

	"github.com/arangodb/go-driver"
	"github.com/golang-jwt/jwt"
	"github.com/infinimesh/infinimesh/pkg/credentials"
	"github.com/infinimesh/infinimesh/pkg/graph/schema"
	inf "github.com/infinimesh/infinimesh/pkg/internal"
	pb "github.com/infinimesh/infinimesh/pkg/node/proto"
	accpb "github.com/infinimesh/infinimesh/pkg/node/proto/accounts"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Account struct {
	*accpb.Account
	driver.DocumentMeta
}

type AccountsController struct {
	pb.UnimplementedAccountsServiceServer
	log *zap.Logger

	col driver.Collection // Accounts Collection
	cred driver.Collection
	db driver.Database

	SIGNING_KEY []byte
}

func NewAccountsController(log *zap.Logger, db driver.Database) AccountsController {
	col, _ := db.Collection(context.TODO(), schema.ACCOUNTS_COL)
	cred, _ := db.Collection(context.TODO(), schema.CREDENTIALS_COL)
	return AccountsController{
		log: log.Named("AccountsController"), col: col, db: db, cred: cred,
		SIGNING_KEY: []byte("just-an-init-thing-replace-me"),
	}
}

//Validate method does the pre-checks for a REST request
func Validate(ctx context.Context, log *zap.Logger) (md metadata.MD, acc string, err error) {

	//Get the metadata from the context
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		log.Error("Failed to get metadata from context")
		return nil, "", status.Error(codes.Aborted, "Failed to get metadata from context")
	}

	//Check for Authentication
	requestorID := md.Get("requestorID")
	if requestorID == nil {
		//Added logging
		log.Error("The account is not authenticated")
		return nil, "", status.Error(codes.Unauthenticated, "The account is not authenticated")
	}
	log.Debug("Requestor ID", zap.Strings("id", requestorID))

	return md, requestorID[0], nil
}

func (c *AccountsController) Token(ctx context.Context, req *pb.TokenRequest) (*pb.TokenResponse, error) {
	log := c.log.Named("Token")
	log.Debug("Token request received", zap.Any("request", req))

	account, ok := c.Authorize(ctx, req.Auth.Type, req.Auth.Data...)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "Wrong credentials given")
	}
	log.Debug("Authorized user", zap.String("ID", account.ID.String()))
	if !account.Enabled {
		return nil, status.Error(codes.PermissionDenied, "Account is disabled")
	}

	claims := jwt.MapClaims{}
	claims[inf.INFINIMESH_ACCOUNT_CLAIM] = account.Key
	claims["exp"] = req.Exp

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token_string, err := token.SignedString(c.SIGNING_KEY)
	if err != nil {
		return nil, status.Error(codes.Internal, "Failed to issue token")
	}

	return &pb.TokenResponse{Token: token_string}, nil
}

func (c *AccountsController) Get(ctx context.Context, acc *accpb.Account) (*accpb.Account, error) {
	log := c.log.Named("Get")
	log.Debug("Get request received", zap.Any("request", acc))

	//Get metadata from context and perform validation
	_, requestor, err := Validate(ctx, log)
	if err != nil {
		return nil, err
	}
	log.Debug("Requestor", zap.String("id", requestor))

	// Getting Account from DB
	_, err = c.col.ReadDocument(ctx, acc.GetUuid(), acc)
	if err != nil {
		log.Error("Error getting account", zap.String("uuid", acc.GetUuid()), zap.Error(err))
		return nil, status.Error(codes.NotFound, "Account not found")
	}

	// Check requestor access to acc.GetUuid()
	
	return acc, nil
}

func (c *AccountsController) Create(ctx context.Context, request *accpb.CreateRequest) (*accpb.CreateResponse, error) {
	log := c.log.Named("Create")
	log.Debug("Create request received", zap.Any("request", request), zap.Any("context", ctx))

	//Get metadata from context and perform validation
	_, requestor, err := Validate(ctx, log)
	if err != nil {
		return nil, err
	}
	log.Debug("Requestor", zap.String("id", requestor))

	// Check requestor access to request.GetNamespace()

	account := Account{Account: request.GetAccount()}
	meta, err := c.col.CreateDocument(ctx, account)
	if err != nil {
		c.log.Debug("Error creating account", zap.Error(err))
		return nil, status.Error(codes.Internal, "Error while creating account")
	}
	account.Uuid = meta.ID.Key()
	account.DocumentMeta = meta

	// Add Account to Namespace

	col, _ := c.db.Collection(ctx, schema.CREDENTIALS_EDGE_COL)
	cred, err := credentials.MakeCredentials(request.GetCredentials(), log)
	if err != nil {
		defer c.col.RemoveDocument(ctx, meta.Key)
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = c.SetCredentialsCtrl(ctx, account, col, cred)
	if err != nil {
		defer c.col.RemoveDocument(ctx, meta.Key)
		return nil, err
	}

	return &accpb.CreateResponse{Account: account.Account}, nil
}

func (c *AccountsController) Update(ctx context.Context, acc *accpb.Account) (*accpb.Account, error) {
	log := c.log.Named("Update")
	log.Debug("Update request received", zap.Any("request", acc), zap.Any("context", ctx))

	//Get metadata from context and perform validation
	_, requestor, err := Validate(ctx, log)
	if err != nil {
		return nil, err
	}
	log.Debug("Requestor", zap.String("id", requestor))

	// Check requestor access to acc.GetUuid()

	_, err = c.col.UpdateDocument(ctx, acc.GetUuid(), acc)
	if err != nil {
		log.Error("Internal error while updating Document", zap.Any("request", acc), zap.Error(err))
		return nil, status.Error(codes.Internal, "Error while updating Account")
	}

	return acc, nil
}

func (c *AccountsController) Delete(ctx context.Context, req *accpb.Account) (*pb.DeleteResponse, error)  {
	log := c.log.Named("Delete")
	log.Debug("Delete request received", zap.Any("request", req), zap.Any("context", ctx))

	//Get metadata from context and perform validation
	_, requestor, err := Validate(ctx, log)
	if err != nil {
		return nil, err
	}
	log.Debug("Requestor", zap.String("id", requestor))

	var acc Account
	c.col.ReadDocument(ctx, req.GetUuid(), &acc)

	// Check requestor access to acc.GetUuid()

	creds, err := c.GetCredentials(ctx, acc)
	if err != nil {
		log.Error("Error gathering Account credentials", zap.String("account", acc.Key), zap.Error(err))
	}
	credsKeys := make([]string, len(creds))
	for i, cred := range creds {
		credsKeys[i] = cred.Key()
	}
	_, errs, err := c.cred.RemoveDocuments(ctx, credsKeys)
	if err != nil {
		log.Error("Error deleting Credentials", zap.String("account", acc.Key), zap.Any("errors", errs), zap.Error(err))
		return nil, status.Error(codes.Internal, "Account has been deleted partialy")
	}

	_, err = c.col.RemoveDocument(ctx, acc.ID.Key())
	if err != nil {
		log.Error("Error deleting Account", zap.String("account", acc.Key), zap.Error(err))
		return nil, status.Error(codes.Internal, "Error deleting Account")
	}

	return &pb.DeleteResponse{}, nil
}

// Helper Functions

func (ctrl *AccountsController) Authorize(ctx context.Context, auth_type string, args ...string) (Account, bool) {
	ctrl.log.Debug("Authorization request", zap.String("type", auth_type))

	credentials, err := credentials.Find(ctx, ctrl.col.Database(), ctrl.log, auth_type, args...)
	// Check if could authorize
	if err != nil {
		ctrl.log.Info("Coudn't authorize", zap.Error(err))
		return Account{}, false
	}

	account, ok := Authorisable(ctx, &credentials, ctrl.col.Database())
	ctrl.log.Debug("Authorized account", zap.Bool("result", ok), zap.Any("account", account))
	return account, ok
}

// Return Account authorisable by this Credentials
func Authorisable(ctx context.Context, cred *credentials.Credentials, db driver.Database) (Account, bool) {
	query := `FOR account IN 1 INBOUND @credentials GRAPH @credentials_graph RETURN account`
	c, err := db.Query(ctx, query, map[string]interface{}{
		"credentials": cred,
		"credentials_graph": schema.CREDENTIALS_GRAPH.Name,
	})
	if err != nil {
		return Account{}, false
	}
	defer c.Close()

	var r Account
	_, err = c.ReadDocument(ctx, &r)
	return r, err == nil
}

// Return Credentials linked to Account
func (ctrl *AccountsController) GetCredentials(ctx context.Context, acc Account) (r []credentials.Credentials, err error) {
	query := `FOR credentials IN 1 OUTBOUND @account GRAPH @credentials_graph RETURN credentials`
	c, err := ctrl.db.Query(ctx, query, map[string]interface{}{
		"account": acc.ID.String(),
		"credentials_graph": schema.CREDENTIALS_GRAPH.Name,
	})
	if err != nil {
		return nil, err
	}
	defer c.Close()

	for {
		var cred credentials.Credentials 
		_, err := c.ReadDocument(ctx, &cred)
		if driver.IsNoMoreDocuments(err) {
			break
		} else if err != nil {
			return nil, err
		}
		r = append(r, cred)
	}
	return r, nil
}

// Set Account Credentials, ensure account has only one credentials document linked per credentials type
func (ctrl *AccountsController) SetCredentialsCtrl(ctx context.Context, acc Account, edge driver.Collection, c credentials.Credentials) (error) {
	key := c.Type() + "-" + acc.Key
	var oldLink credentials.Link
	_, err := edge.ReadDocument(ctx, key, &oldLink)
	if err == nil {	
		_, err = ctrl.cred.UpdateDocument(ctx, oldLink.To.Key(), c)
		if err != nil {
			ctrl.log.Error("Error updating Credentials of type", zap.Error(err), zap.String("key", key))
			return status.Error(codes.InvalidArgument, "Error updating Credentials of type")
		}

		return nil
	}
	ctrl.log.Debug("Credentials of type don't exist or failed to Read then from DB", zap.Error(err), zap.String("key", key))

	cred, err := ctrl.cred.CreateDocument(ctx, c)	
	if err != nil {
		ctrl.log.Error("Error creating Credentials Document", zap.String("type", c.Type()), zap.Error(err))
		return status.Error(codes.Internal, "Couldn't create credentials")
	}

	_, err = edge.CreateDocument(ctx, credentials.Link{
		From: acc.ID,
		To: cred.ID,
		Type: c.Type(),
		DocumentMeta: driver.DocumentMeta {
			Key: key, // Ensure only one credentials vertex per type
		},
	})
	if err != nil {
		ctrl.log.Error("Error Linking Credentials to Account",
			zap.String("account", acc.Key), zap.String("type", c.Type()), zap.Error(err),
		)
		ctrl.cred.RemoveDocument(ctx, cred.Key)
		return status.Error(codes.Internal, "Couldn't assign credentials")
	}
	return nil
}

func (c *AccountsController) SetCredentials(ctx context.Context, req *pb.SetCredentialsRequest) (*pb.SetCredentialsResponse, error) {
	log := c.log.Named("SetCredentials")
	log.Debug("Set Credentials request received", zap.String("account", req.GetUuid()), zap.String("type", req.GetCredentials().GetType()), zap.Any("context", ctx))

	//Get metadata from context and perform validation
	_, requestor, err := Validate(ctx, log)
	if err != nil {
		return nil, err
	}
	log.Debug("Requestor", zap.String("id", requestor))

	var acc Account
	c.col.ReadDocument(ctx, req.GetUuid(), &acc)

	// Check requestor access to acc.GetUuid()

	col, _ := c.db.Collection(ctx, schema.CREDENTIALS_EDGE_COL)
	cred, err := credentials.MakeCredentials(req.GetCredentials(), log)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = c.SetCredentialsCtrl(ctx, acc, col, cred)
	if err != nil {
		return nil, err
	}
	return &pb.SetCredentialsResponse{}, nil
}