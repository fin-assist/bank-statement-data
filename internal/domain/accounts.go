package domain

const (
	// AccountKindChecking identifies a checking account in stored source facts.
	AccountKindChecking = "checking"
	// AccountKindSavings identifies a savings account in stored source facts.
	AccountKindSavings = "savings"
	// AccountKindCredit identifies a credit account in stored source facts.
	AccountKindCredit = "credit"
)

const (
	// AccountNameChecking is the public display name of a checking account.
	AccountNameChecking = "Текущий"
	// AccountNameSavings is the public display name of a savings account.
	AccountNameSavings = "Накопительный"
	// AccountNameCredit is the public display name of a credit account.
	AccountNameCredit = "Кредитный" // #nosec G101 -- public account display name, not a credential.
	// AccountNameGeneric is the fallback display name when the type is unknown.
	AccountNameGeneric = "Счёт"
	// AccountNameOtherBank is the public name of an account at another bank.
	AccountNameOtherBank = "Счёт другого банка"
)
