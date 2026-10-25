-- The Razorpay poller looks at payment links nobody has settled yet, least recently checked first,
-- so one stubborn link cannot starve the others. checked_at is when it last looked.
ALTER TABLE razorpay_links ADD COLUMN checked_at timestamptz;
CREATE INDEX razorpay_links_created_idx ON razorpay_links (created_at);
