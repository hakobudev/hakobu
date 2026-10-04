-- Apps whose new build went live on a server that dropped off before it
-- became the Latest image there: the panel finishes it (node Promote) as
-- the server connects again, so Latest is the build that serves.
CREATE TABLE pending_promotions (
	app_name TEXT PRIMARY KEY
);
