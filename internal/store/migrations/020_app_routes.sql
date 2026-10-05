-- Paths of an app's address served by another app of its project, through
-- the tunnel: shop.example.com/api/... goes to the api app, the rest to
-- the shop. path is a prefix like /api; the target sees the whole path.
CREATE TABLE app_routes (
	app_name TEXT NOT NULL,
	path TEXT NOT NULL,
	target TEXT NOT NULL,
	PRIMARY KEY (app_name, path)
);
