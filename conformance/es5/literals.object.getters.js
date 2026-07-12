function check(a,b){if(a!==b)throw a+" !== "+b;} var o={_x:5,get x(){return this._x;}};check(o.x,5);var g={get val(){return 42;}};check(g.val,42);console.log('PASS');
