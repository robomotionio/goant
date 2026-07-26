// Method lookup on a prototype plus the call itself.
function Point(x, y) { this.x = x; this.y = y; }
Point.prototype.dot = function (o) { return this.x * o.x + this.y * o.y; };
var a = new Point(1, 2), b = new Point(3, 4);
var sum = 0;
for (var i = 0; i < 300000; i++) sum += a.dot(b);
RESULT = sum;
