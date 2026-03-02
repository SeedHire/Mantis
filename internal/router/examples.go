package router

// LabeledExample is a (query, tier) training pair for the embedding classifier.
type LabeledExample struct {
	Query string
	Tier  Tier
}

// RouterExamples is the labeled dataset used to train the kNN classifier.
// These cover the main misfire categories and the happy-path cases from router_test.go.
var RouterExamples = []LabeledExample{
	// ── TierTrivial ──────────────────────────────────────────────────────────
	{Query: "what is a goroutine", Tier: TierTrivial},
	{Query: "define struct in go", Tier: TierTrivial},
	{Query: "syntax for switch statement", Tier: TierTrivial},
	{Query: "what does defer mean", Tier: TierTrivial},
	{Query: "meaning of nil", Tier: TierTrivial},
	{Query: "how to declare a variable", Tier: TierTrivial},
	{Query: "what is an interface", Tier: TierTrivial},
	{Query: "rename this function", Tier: TierTrivial},
	{Query: "fix this typo", Tier: TierTrivial},
	{Query: "one line to reverse a string", Tier: TierTrivial},
	{Query: "autocomplete this function signature", Tier: TierTrivial},
	{Query: "what's the syntax for slice literal", Tier: TierTrivial},

	// ── TierFast ─────────────────────────────────────────────────────────────
	{Query: "what does this function do", Tier: TierFast},
	{Query: "explain this line of code", Tier: TierFast},
	{Query: "how to use fmt.Sprintf", Tier: TierFast},
	{Query: "show me an example of error wrapping", Tier: TierFast},
	{Query: "what does context.WithTimeout do", Tier: TierFast},
	{Query: "what is the difference between make and new", Tier: TierFast},
	{Query: "how to iterate over a map", Tier: TierFast},
	{Query: "show me a goroutine example", Tier: TierFast},
	{Query: "what type does json.Unmarshal expect", Tier: TierFast},
	{Query: "quick example of http handler", Tier: TierFast},
	{Query: "how do I read a file in go", Tier: TierFast},
	{Query: "what does this error mean", Tier: TierFast},

	// ── TierCode ─────────────────────────────────────────────────────────────
	{Query: "implement a retry function with exponential backoff", Tier: TierCode},
	{Query: "write a function to validate email addresses", Tier: TierCode},
	{Query: "add pagination to the users list endpoint", Tier: TierCode},
	{Query: "fix the null pointer in auth.go", Tier: TierCode},
	{Query: "refactor the login handler to use the new session package", Tier: TierCode},
	{Query: "add unit tests for the payment processor", Tier: TierCode},
	{Query: "implement rate limiting middleware", Tier: TierCode},
	{Query: "write a parser for the CSV format", Tier: TierCode},
	{Query: "create a database migration for the users table", Tier: TierCode},
	{Query: "debug why the websocket connection drops", Tier: TierCode},
	{Query: "add error handling to the file upload handler", Tier: TierCode},
	{Query: "build a button component", Tier: TierCode},
	{Query: "add a loading spinner to the form", Tier: TierCode},
	{Query: "write a helper to format currency", Tier: TierCode},
	{Query: "implement the user profile page", Tier: TierCode},
	{Query: "fix the race condition in the cache", Tier: TierCode},
	{Query: "add validation to the signup form", Tier: TierCode},
	{Query: "create a middleware to log request durations", Tier: TierCode},
	{Query: "write a script to seed the database", Tier: TierCode},
	{Query: "optimise the SQL query in the dashboard", Tier: TierCode},
	{Query: "add a dark mode toggle", Tier: TierCode},
	{Query: "implement search with debouncing", Tier: TierCode},
	{Query: "write a webhook handler for Stripe events", Tier: TierCode},
	{Query: "fix the broken test in session_test.go", Tier: TierCode},

	// ── TierReason ───────────────────────────────────────────────────────────
	{Query: "why does my mutex deadlock in this goroutine", Tier: TierReason},
	{Query: "explain how the authentication flow works", Tier: TierReason},
	{Query: "what are the tradeoffs between redis and sqlite for sessions", Tier: TierReason},
	{Query: "when should I use a channel versus a mutex", Tier: TierReason},
	{Query: "how does the BFS traversal work in the graph package", Tier: TierReason},
	{Query: "explain the architecture of the router package", Tier: TierReason},
	{Query: "what is the best approach to handle auth across microservices", Tier: TierReason},
	{Query: "why is this code slow — analyse the bottleneck", Tier: TierReason},
	{Query: "how should I structure the database access layer", Tier: TierReason},
	{Query: "explain the whole session management system", Tier: TierReason},
	{Query: "what design pattern should I use for the event bus", Tier: TierReason},
	{Query: "analyse the dependencies in the auth module", Tier: TierReason},
	{Query: "why does the test fail intermittently", Tier: TierReason},
	{Query: "how does context propagation work across goroutines", Tier: TierReason},
	{Query: "pros and cons of using an ORM versus raw SQL here", Tier: TierReason},
	{Query: "what is the difference between the two cache implementations", Tier: TierReason},

	// ── TierHeavy ────────────────────────────────────────────────────────────
	{Query: "rewrite the entire payment module to use the new gateway", Tier: TierHeavy},
	{Query: "migrate the authentication system from JWT to sessions", Tier: TierHeavy},
	{Query: "refactor all files in the user package to use the new error type", Tier: TierHeavy},
	{Query: "scaffold a complete REST API with auth and database", Tier: TierHeavy},
	{Query: "build a full backend for the e-commerce app", Tier: TierHeavy},
	{Query: "set up the entire project structure from scratch", Tier: TierHeavy},
	{Query: "create a full-stack todo application with react and go", Tier: TierHeavy},
	{Query: "implement the whole notification service", Tier: TierHeavy},
	{Query: "refactor the codebase to use the repository pattern", Tier: TierHeavy},
	{Query: "rewrite the data pipeline to use streaming", Tier: TierHeavy},
	{Query: "set up a microservice for order processing", Tier: TierHeavy},
	{Query: "build a GraphQL API for the existing REST backend", Tier: TierHeavy},
	{Query: "create the entire user management system", Tier: TierHeavy},

	// ── TierMax ──────────────────────────────────────────────────────────────
	{Query: "do a full security audit of the authentication code", Tier: TierMax},
	{Query: "comprehensive code review of the payment module", Tier: TierMax},
	{Query: "find all bugs in this codebase", Tier: TierMax},
	{Query: "what is the best approach to scale this system", Tier: TierMax},
	{Query: "compare all the approaches for implementing caching here", Tier: TierMax},
	{Query: "deep dive into the performance issues across all services", Tier: TierMax},
	{Query: "should I use postgres or mongodb for this use case", Tier: TierMax},
	{Query: "review all the security vulnerabilities before deployment", Tier: TierMax},
	{Query: "is this production ready", Tier: TierMax},
	{Query: "evaluate all the tradeoffs in the current architecture", Tier: TierMax},
}
