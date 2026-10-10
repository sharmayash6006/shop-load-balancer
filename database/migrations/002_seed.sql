-- Seed catalogue only. No users, no orders, no metrics: those are produced by real traffic.
-- Stock is deliberately large so that load tests do not run the shop out of inventory.
INSERT INTO products (name, description, price, stock, category, image) VALUES
 ('Laptop Pro 15',   '15-inch laptop, 16 GB RAM, 512 GB SSD, all-day battery.',        65000.00, 100000, 'Computers',   '💻'),
 ('Smartphone X',    '6.5-inch OLED display, 128 GB storage, dual camera.',            32999.00, 100000, 'Phones',      '📱'),
 ('Noise-Cancel Headphones', 'Over-ear wireless headphones with active noise cancelling.', 8999.00, 100000, 'Audio',   '🎧'),
 ('Mechanical Keyboard', 'Hot-swappable mechanical keyboard with RGB backlight.',      4499.00, 100000, 'Accessories', '⌨️'),
 ('Wireless Mouse',  'Ergonomic wireless mouse, 4000 DPI, 6 buttons.',                  1299.00, 100000, 'Accessories', '🖱️'),
 ('4K Monitor 27"',  '27-inch 4K IPS monitor, 60 Hz, USB-C power delivery.',           24999.00, 100000, 'Computers',   '🖥️'),
 ('Smart Watch',     'Fitness tracker with heart-rate sensor, GPS and 7-day battery.', 12999.00, 100000, 'Wearables',   '⌚'),
 ('Mirrorless Camera','24 MP mirrorless camera with 4K video and kit lens.',           54999.00, 100000, 'Cameras',     '📷');
