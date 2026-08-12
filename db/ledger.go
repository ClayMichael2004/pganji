package db

import(
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TransferParams struct{
	FromAccountID  string
	ToAccountID string
	Amount  int64 //bigint/cents
	Reference  string //unique business tracking refrence
	Description  string
}

func GetAccountbalance(ctx context.Context, tx pgx.Tx, accountID string)(int64, error){
	query:= `
	SELECT
	COALESCE(SUM(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE 0 END), 0) - COALESCE(SUM(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE 0 END), 0)
	AS balance
	FROM journal_entries
	WHERE account_id = $1;
	`
	var balance int64
	//tx are used to manage transactions ie pgx.tx
	err:= tx.QueryRow(ctx, query, accountID).Scan(&balance)
	if err !=nil{
		return 0, fmt.Errorf("failed to calculate balance: %w", err)
	}
	return balance, nil
}

func TransferTx(ctx context.Context, pool *pgxpool.Pool, params TransferParams)error{
	//begin the database transaction
	tx, err:= pool.Begin(ctx)
	if err!=nil{
		return fmt.Errorf("failed to begin transaction %w", err)
	}
	//if the function return early due to error it rolls back. if tx.Commit() succeeeds later then rollback does nothing
	defer tx.Rollback(ctx)

	//lock accounts in deterministic order to prevent deadlocks
	//always lock the smaller uuid's forst spo the 2 concurrent transfers dont lock each other out
	firstID, secondID:= params.FromAccountID, params.ToAccountID
	if firstID> secondID{
		firstID, secondID=secondID, firstID
	}

	lockQuery:= `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE;`
	rows, err:= tx.Query(ctx, lockQuery, firstId, secondID)
	if err!=nil{
		return fmt.Errorf("failed to aqcuire row locks: %w", err)
	}
	rows.Close()

	//check the sender balance
	senderBalance, err:= GetAccountbalance(ctx, tx, params.FromAccountID)
	if err!=nil{
		return err
	}

	if senderBalance< params.Amount{
		return fmt.Errorf("insufficient funds: available %d, required %d", senderBalance, params.Amount)
	}

	//creating the transaction header record
	var txID string
	createTxQuery:= `
	INSERT INTO transactions (reference, description, status)
	VALUES ($1, $2, 'COMPLETED)
	RETURNING id;
	`

	err=tx.QueryRow(ctx, createTxQuery, params.Reference, params.Description).Scan(&txID)
	if err!=nil{
		return fmt.Errorf("failed to create transaction record: %w", err)
	}

	//creating the debit entry for the sender: decreasing his amount/ASSET COUNT

	insertEntryQuery:= `
	INSERT INTO journal_entries(transaction_id, account_id, entry_type, amount)
	VALUES($1, $2, $3, $4);
	`

	_, err=tx.Exec(ctx, insertEntryQuery, txID, params.FromAccountID, "CREDIT", params.Amount)
	if err!=nil{
		return fmt.Errorf("failed to insert sender entry %w", err)
	}

	//creating the credit entry for the receiver: increasing his amount/asset count
	_, err=tx.Exec(ctx, insertEntryQuery, txID, params.ToAccountID, "DEBIT", params.Amount)
	if err!=nil{
		return fmt.Errorf("failed to insert receiver entry %w", err)
	}

	//commit transaction/permanenlty save all the changes using tx.Commit(ctx)as we said earlier
	if err:= tx.Commit(ctx); err!=nil{
		return fmt.Errorf("failed to commit transaction %w", err)
	}
	return nil
}