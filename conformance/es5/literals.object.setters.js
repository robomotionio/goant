function check(a,b){if(a!==b)throw a+" !== "+b;} var o={_x:0,set x(v){this._x=v*2;}};o.x=5;check(o._x,10);console.log('PASS');
