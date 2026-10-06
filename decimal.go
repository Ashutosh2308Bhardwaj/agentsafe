package agentsafe

// Decimal is an exact decimal number written as text: "4200.50". Use it for money taken from a CSV, a DECIMAL
// column or a JSON string, so it is compared by value (Decimal("4200.50") equals 4200.5 and Decimal("4200.5"))
// rather than as text, and never through a binary float.
type Decimal string
